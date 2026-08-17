package tftp

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/internal/capture"
)

// resolvingMemFS is a memFS that can also report a path's backing image,
// so the recorded transfer carries the image/in-image path.
type resolvingMemFS struct {
	memFS
	images map[string]Resolved
}

func (m resolvingMemFS) ResolveImage(path string) (Resolved, error) {
	r, ok := m.images[path]
	if !ok {
		return Resolved{}, ErrNotFound
	}
	return r, nil
}

// waitForEvent polls the capture's events.jsonl until an event with the
// given name appears (the server records from its own goroutine after the
// client is done), then returns it.
func waitForEvent(t *testing.T, dir, name string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f, err := os.Open(filepath.Join(dir, "events.jsonl"))
		if err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				var m map[string]any
				if json.Unmarshal(sc.Bytes(), &m) == nil && m["event"] == name {
					f.Close()
					return m
				}
			}
			f.Close()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("event %q never appeared in %s", name, dir)
	return nil
}

func TestServeFileRecordsTransfer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	rec, err := capture.New(dir)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	defer rec.Close()

	content := bytes.Repeat([]byte("z"), 1024)
	fs := resolvingMemFS{
		memFS:  memFS{"stand/fx.64": content},
		images: map[string]Resolved{"stand/fx.64": {Image: "tools.iso", Path: "stand/fx.64"}},
	}
	addr := startServer(t, &Server{FS: fs, Recorder: rec})

	got, _ := fetch(t, addr, "stand/fx.64")
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch")
	}

	e := waitForEvent(t, dir, "tftp_transfer_end")
	if e["requested_name"] != "stand/fx.64" {
		t.Errorf("requested_name = %v", e["requested_name"])
	}
	if e["result"] != "ok" {
		t.Errorf("result = %v, want ok", e["result"])
	}
	if e["size"] != float64(1024) {
		t.Errorf("size = %v, want 1024", e["size"])
	}
	if e["bytes_sent"] != float64(1024) {
		t.Errorf("bytes_sent = %v, want 1024", e["bytes_sent"])
	}
	if e["image"] != "tools.iso" {
		t.Errorf("image = %v, want tools.iso", e["image"])
	}
	if bs, ok := e["block_size"].(float64); !ok || bs != 512 {
		t.Errorf("block_size = %v, want 512", e["block_size"])
	}
}

// TestServeFileRecordsUnackedFinalBlock covers the normal SGI PROM
// behavior: the client stops listening once it has the file and never
// acks the last block. That is not a failure, so it gets its own result,
// and the transmitted byte count includes the final block even though the
// acknowledged count does not.
func TestServeFileRecordsUnackedFinalBlock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	rec, err := capture.New(dir)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	defer rec.Close()

	content := bytes.Repeat([]byte("z"), 700) // 512 (full) + 188 (final)
	srv := &Server{FS: memFS{"f": content}, Recorder: rec, FinalTimeout: 30 * time.Millisecond, FinalRetries: 1}
	addr := startServer(t, srv)
	fetchNoFinalAck(t, addr, "f")

	e := waitForEvent(t, dir, "tftp_transfer_end")
	if e["result"] != "unacked_final" {
		t.Errorf("result = %v, want unacked_final", e["result"])
	}
	if e["bytes_sent"] != float64(700) {
		t.Errorf("bytes_sent (transmitted) = %v, want 700", e["bytes_sent"])
	}
	if e["bytes_acked"] != float64(512) {
		t.Errorf("bytes_acked = %v, want 512", e["bytes_acked"])
	}
}

// fetchNoFinalAck reads a transfer but acks only the full (512-byte)
// blocks, going silent on the first short block - the final one - to
// mimic an SGI PROM that stops listening once it has the file.
func fetchNoFinalAck(t *testing.T, addr *net.UDPAddr, file string) {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.WriteTo(rrq(file, "octet"), addr); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	for {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, from, err := c.ReadFrom(buf)
		if err != nil {
			return
		}
		if binary.BigEndian.Uint16(buf[0:2]) != 3 {
			return
		}
		payload := n - 4
		if payload < 512 {
			return // final block: leave it unacked
		}
		block := binary.BigEndian.Uint16(buf[2:4])
		ack := make([]byte, 4)
		binary.BigEndian.PutUint16(ack, 4)
		binary.BigEndian.PutUint16(ack[2:], block)
		c.WriteTo(ack, from)
	}
}

func TestServeFileRecordsClientAlias(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	rec, err := capture.New(dir)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	defer rec.Close()
	srv := &Server{
		FS:         memFS{"f": []byte("x")},
		Recorder:   rec,
		ClientName: func(netip.Addr) string { return "octane" },
	}
	addr := startServer(t, srv)
	fetch(t, addr, "f")

	e := waitForEvent(t, dir, "tftp_transfer_end")
	if e["client"] != "octane" {
		t.Errorf("client = %v, want the configured alias octane, not an IP", e["client"])
	}
}

func TestServeFileRecordsNotFound(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	rec, err := capture.New(dir)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	defer rec.Close()

	addr := startServer(t, &Server{FS: memFS{}, Recorder: rec})
	// request a missing file; the server replies with a TFTP ERROR and
	// records the transfer as notfound
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.WriteTo(rrq("nope", "octet"), addr); err != nil {
		t.Fatal(err)
	}

	e := waitForEvent(t, dir, "tftp_transfer_end")
	if e["result"] != "notfound" {
		t.Errorf("missing-file result = %v, want notfound", e["result"])
	}
}
