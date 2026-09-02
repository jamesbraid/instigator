package serve

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/internal/capture"
)

// readEvents decodes every event line in a capture's events.jsonl.
func readEvents(t *testing.T, dir string) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("open events.jsonl: %v", err)
	}
	defer f.Close()
	var evs []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			evs = append(evs, m)
		}
	}
	return evs
}

// waitForCounts polls until events.jsonl holds at least the wanted count of
// each named event - the protocol servers record from their own goroutines
// after the client is done, so the test cannot read immediately.
func waitForCounts(t *testing.T, dir string, want map[string]int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got := map[string]int{}
		for _, e := range readEvents(t, dir) {
			got[e["event"].(string)]++
		}
		ok := true
		for name, n := range want {
			if got[name] < n {
				ok = false
			}
		}
		if ok {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("events %v never all appeared in %s", want, dir)
}

func firstEvent(evs []map[string]any, name string) map[string]any {
	for _, e := range evs {
		if e["event"] == name {
			return e
		}
	}
	return nil
}

// TestCaptureRecordsFullInstall is the recorder's end-to-end test: it
// drives BOOTP, TFTP, and an rsh shell dd through one running server with
// capture on, then asserts the bundle - run.json provenance, the full event
// set in events.jsonl, and the derived summary.json and human report.
func TestCaptureRecordsFullInstall(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	sink := &syncBuffer{}
	rec, err := capture.New(dir, capture.WithSummaryWriter(sink))
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}

	// One socket both sends the BOOTP request and receives its reply.
	bootpC, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer bootpC.Close()

	cfg := testConfig(t)
	s, err := Start(cfg, testLogger(t),
		withRSHHighPorts(),
		withBootpReplyAddr(bootpC.LocalAddr()),
		withRecorder(rec),
	)
	if err != nil {
		t.Fatal(err)
	}

	const fxPath = "/6.5.30/stand/fx.64"
	const saPath = "/6.5.30/dist/sa"

	// --- BOOTP ---
	req := make([]byte, 300)
	req[0], req[1], req[2] = 1, 1, 6
	copy(req[28:], []byte{0x08, 0x00, 0x69, 0x0e, 0xaf, 0x12})
	copy(req[108:], []byte(fxPath))
	if _, err := bootpC.WriteTo(req, sendAddr(s.BOOTPAddr())); err != nil {
		t.Fatal(err)
	}
	rbuf := make([]byte, 400)
	bootpC.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := bootpC.ReadFrom(rbuf); err != nil {
		t.Fatalf("bootp reply: %v", err)
	}

	// --- TFTP fx.64 ---
	fetchTFTP(t, s.TFTPAddr(), fxPath)

	// --- rsh shell: exec /bin/sh, then dd the sa partitioner over stdin ---
	rc, err := net.Dial("tcp", s.RSHAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(rc, "0\x00guest\x00guest\x00exec /bin/sh\x00")
	io.WriteString(rc, "dd if="+saPath+" bs=512\n")
	rc.(*net.TCPConn).CloseWrite() // EOF on stdin ends the shell
	io.ReadAll(rc)
	rc.Close()

	waitForCounts(t, dir, map[string]int{
		"bootp_reply": 1, "tftp_transfer_end": 1, "inst_command_end": 1, "rsh_session_end": 1,
	})

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// --- run.json provenance (private) ---
	fi, err := os.Stat(filepath.Join(dir, "run.json"))
	if err != nil {
		t.Fatalf("stat run.json: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("run.json perm = %o, want 600", fi.Mode().Perm())
	}
	rb, _ := os.ReadFile(filepath.Join(dir, "run.json"))
	var run map[string]any
	if err := json.Unmarshal(rb, &run); err != nil {
		t.Fatalf("run.json invalid: %v", err)
	}
	if run["run_id"] == nil || run["run_id"] == "" {
		t.Error("run.json missing run_id")
	}
	if media, ok := run["media"].([]any); !ok || len(media) == 0 {
		t.Errorf("run.json media manifest empty: %v", run["media"])
	}

	// --- events.jsonl: full lifecycle, no unexpected listener exit ---
	evs := readEvents(t, dir)
	// server_start must be the very first event: the serve loops are
	// launched only after it is emitted, so no protocol event can precede it.
	if len(evs) == 0 || evs[0]["event"] != "server_start" {
		t.Errorf("first event = %v, want server_start", func() any {
			if len(evs) == 0 {
				return nil
			}
			return evs[0]["event"]
		}())
	}
	for _, name := range []string{"server_start", "bootp_reply", "tftp_transfer_end", "rsh_session_start", "inst_command_end", "rsh_session_end", "server_stop"} {
		if firstEvent(evs, name) == nil {
			t.Errorf("events.jsonl missing %q", name)
		}
	}
	if e := firstEvent(evs, "listener_exit"); e != nil {
		t.Errorf("unexpected listener_exit recorded: %v", e)
	}

	bootp := firstEvent(evs, "bootp_reply")
	if bootp["result"] != "answered" || bootp["client"] != "octane" {
		t.Errorf("bootp_reply = %v, want answered/octane", bootp)
	}

	tftp := firstEvent(evs, "tftp_transfer_end")
	if tftp["result"] != "ok" || tftp["image"] != "base" {
		t.Errorf("tftp_transfer_end = %v, want result ok image base(layer)", tftp)
	}

	cmd := firstEvent(evs, "inst_command_end")
	if cmd["verb"] != "dd" {
		t.Errorf("command verb = %v, want dd", cmd["verb"])
	}
	if cmd["efs_read_bytes"].(float64) != 1024 {
		t.Errorf("dd efs_read_bytes = %v, want 1024", cmd["efs_read_bytes"])
	}
	if cmd["stdout_bytes"].(float64) < 1024 {
		t.Errorf("dd stdout_bytes = %v, want >= 1024", cmd["stdout_bytes"])
	}
	served, ok := cmd["served"].([]any)
	if !ok || len(served) == 0 || served[0].(map[string]any)["image"] != "base" {
		t.Errorf("dd served entry = %v, want layer base", cmd["served"])
	}

	// --- summary.json + human report ---
	sb, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatalf("read summary.json: %v", err)
	}
	var sum capture.Summary
	if err := json.Unmarshal(sb, &sum); err != nil {
		t.Fatalf("summary.json invalid: %v", err)
	}
	if sum.BytesServed <= 0 {
		t.Errorf("summary bytes served = %d, want > 0", sum.BytesServed)
	}
	if sum.Commands < 1 || sum.Transfers < 1 {
		t.Errorf("summary commands=%d transfers=%d, want >=1 each", sum.Commands, sum.Transfers)
	}
	if sink.String() == "" {
		t.Error("no human summary written at shutdown")
	}
}

// TestCaptureRecordsIdleSession proves the wired rsh idle timeout: a client
// that opens a shell and then goes silent is cut off and recorded as idle,
// rather than holding the session open forever.
func TestCaptureRecordsIdleSession(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	rec, err := capture.New(dir)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	cfg := testConfig(t)
	s, err := Start(cfg, testLogger(t),
		withRSHHighPorts(),
		withRSHIdleTimeout(100*time.Millisecond),
		withRecorder(rec),
	)
	if err != nil {
		t.Fatal(err)
	}

	rc, err := net.Dial("tcp", s.RSHAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	// Open a shell, then send nothing: the stdin read should idle out.
	fmt.Fprint(rc, "0\x00guest\x00guest\x00exec /bin/sh\x00")
	io.ReadAll(rc) // blocks until the server closes the idled session
	rc.Close()

	waitForCounts(t, dir, map[string]int{"rsh_session_end": 1})
	s.Close()

	end := firstEvent(readEvents(t, dir), "rsh_session_end")
	if end["result"] != "idle" {
		t.Errorf("silent session result = %v, want idle", end["result"])
	}
}

// TestCloseCancelsSlowTransfer covers force-cancellation: a client that
// requests a multi-block file and never acks would keep the server in its
// retry budget for seconds. Close must close the transfer socket and cancel
// it promptly, recording the aborted transfer rather than waiting it out.
func TestCloseCancelsSlowTransfer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	rec, err := capture.New(dir)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	cfg := testConfig(t)
	s, err := Start(cfg, testLogger(t), withRSHHighPorts(), withRecorder(rec))
	if err != nil {
		t.Fatal(err)
	}

	// request dist/sa (multi-block) and read the first block but never ack:
	// the server enters its retransmit loop.
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var rrq []byte
	rrq = binary.BigEndian.AppendUint16(rrq, 1)
	rrq = append(rrq, "/6.5.30/dist/sa"...)
	rrq = append(rrq, 0)
	rrq = append(rrq, "octet"...)
	rrq = append(rrq, 0)
	c.WriteTo(rrq, sendAddr(s.TFTPAddr()))
	buf := make([]byte, 1500)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := c.ReadFrom(buf); err != nil { // first DATA block, left unacked
		t.Fatalf("read first block: %v", err)
	}

	start := time.Now()
	s.Close() // may return a non-nil incomplete error; the point is speed
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Close took %s; the retrying transfer was not force-cancelled", elapsed)
	}
	if firstEvent(readEvents(t, dir), "tftp_transfer_end") == nil {
		t.Error("cancelled transfer was not recorded")
	}
}

func TestFailedRunJSONAbortsStart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	rec, err := capture.New(dir)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	// make run.json unwritable by occupying its path with a directory
	if err := os.Mkdir(filepath.Join(dir, "run.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	if _, err := Start(cfg, testLogger(t), withRSHHighPorts(), withRecorder(rec)); err == nil {
		t.Fatal("Start succeeded despite run.json being unwritable")
	}
}

func TestCloseReturnsRecorderError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	rec, err := capture.New(dir)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	cfg := testConfig(t)
	s, err := Start(cfg, testLogger(t), withRSHHighPorts(), withRecorder(rec))
	if err != nil {
		t.Fatal(err)
	}
	// occupy summary.json so Finish's write fails
	if err := os.Mkdir(filepath.Join(dir, "summary.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err == nil {
		t.Error("Close returned nil despite the recorder failing to finalize")
	}
}

// readUntil reads from c until sub appears or a deadline passes.
func readUntil(t *testing.T, c net.Conn, sub string) {
	t.Helper()
	var got []byte
	buf := make([]byte, 4096)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		n, err := c.Read(buf)
		got = append(got, buf[:n]...)
		if bytes.Contains(got, []byte(sub)) {
			return
		}
		if err != nil {
			t.Fatalf("read until %q: %v (got %q)", sub, err, got)
		}
	}
}

// TestCaptureDrainsActiveSessionOnClose covers a shutdown mid-install: an
// rsh session is still open (the client sent a command and is idle, not
// disconnected) when the server stops. Its end events must be recorded,
// not lost to a recorder finalized while the session was still live.
func TestCaptureDrainsActiveSessionOnClose(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	rec, err := capture.New(dir)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	cfg := testConfig(t)
	s, err := Start(cfg, testLogger(t), withRSHHighPorts(), withRecorder(rec))
	if err != nil {
		t.Fatal(err)
	}

	rc, err := net.Dial("tcp", s.RSHAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	fmt.Fprint(rc, "0\x00guest\x00guest\x00exec /bin/sh\x00")
	io.WriteString(rc, "dd if=/6.5.30/dist/sa bs=512\n")
	// wait for dd to finish (its records summary is the last thing it
	// writes), then leave the session open and idle.
	readUntil(t, rc, "records out")

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	evs := readEvents(t, dir)
	if firstEvent(evs, "inst_command_end") == nil {
		t.Error("the completed command's end event was lost at shutdown")
	}
	if firstEvent(evs, "rsh_session_end") == nil {
		t.Error("the active session's end event was lost: Close finalized before draining it")
	}
}

// TestCaptureRecordsRejectedRSHConnection covers the reserved-port
// decision: a client connecting from a non-reserved source port is refused
// by rcmd before any session begins, and that refusal must be recorded -
// on hardware it is exactly the kind of silent stall the recorder exists
// to surface.
func TestCaptureRecordsRejectedRSHConnection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	rec, err := capture.New(dir)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	cfg := testConfig(t)
	// No withRSHHighPorts: the test's ephemeral (high) source port is refused.
	s, err := Start(cfg, testLogger(t), withRecorder(rec))
	if err != nil {
		t.Fatal(err)
	}

	rc, err := net.Dial("tcp", s.RSHAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(rc, "0\x00guest\x00guest\x00exec /bin/sh\x00")
	io.ReadAll(rc) // server refuses and closes
	rc.Close()

	waitForCounts(t, dir, map[string]int{"rsh_session_end": 1})
	s.Close()

	end := firstEvent(readEvents(t, dir), "rsh_session_end")
	if end == nil || end["result"] != "refused" {
		t.Errorf("rejected connection result = %v, want refused", end)
	}
}

// TestFailedStartDoesNotClaimACleanRun covers the lifecycle nit: if a
// listener fails to bind, the run never really started, so the trace must
// not record a clean start/stop or write a summary.
func TestFailedStartDoesNotClaimACleanRun(t *testing.T) {
	// Bind the wildcard, matching how the rsh listener binds (":port"). A
	// 127.0.0.1-only bind here would not collide with the server's wildcard
	// bind on macOS, so Start would unexpectedly succeed.
	occupied, err := net.Listen("tcp4", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	dir := filepath.Join(t.TempDir(), "cap")
	rec, err := capture.New(dir)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	cfg := testConfig(t)
	cfg.Ports.RSH = occupied.Addr().(*net.TCPAddr).Port // force the rsh bind to fail

	if _, err := Start(cfg, testLogger(t), withRSHHighPorts(), withRecorder(rec)); err == nil {
		t.Fatal("expected Start to fail on the occupied rsh port")
	}

	for _, e := range readEvents(t, dir) {
		if e["event"] == "server_start" || e["event"] == "server_stop" {
			t.Errorf("recorded %v for a server that never started", e["event"])
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "summary.json")); err == nil {
		t.Error("summary.json written for a run that never started")
	}
}

// fetchTFTP runs a minimal client-side TFTP read of a small file.
func fetchTFTP(t *testing.T, addr net.Addr, path string) {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var rrq []byte
	rrq = binary.BigEndian.AppendUint16(rrq, 1) // RRQ
	rrq = append(rrq, path...)
	rrq = append(rrq, 0)
	rrq = append(rrq, "octet"...)
	rrq = append(rrq, 0)
	if _, err := c.WriteTo(rrq, sendAddr(addr)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, from, err := c.ReadFrom(buf)
	if err != nil {
		t.Fatalf("tftp read: %v", err)
	}
	if op := binary.BigEndian.Uint16(buf[0:2]); op != 3 {
		t.Fatalf("tftp opcode %d body %q", op, buf[4:n])
	}
	// ack the (single, small) block so the server completes the transfer
	ack := make([]byte, 4)
	binary.BigEndian.PutUint16(ack, 4)
	binary.BigEndian.PutUint16(ack[2:], 1)
	c.WriteTo(ack, from)
}
