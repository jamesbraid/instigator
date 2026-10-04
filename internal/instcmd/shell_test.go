package instcmd

import (
	"bytes"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/internal/logging"
	"mvdan.cc/sh/v3/syntax"
)

// shellTestFS builds a fixture with a distinctive (non-uniform) byte
// pattern, so a byte-exact check actually catches truncation or
// reordering instead of passing on any file full of one repeated byte.
func shellTestFS() *fakeFS {
	content := make([]byte, 2000)
	for i := range content {
		content[i] = byte(i % 251)
	}
	return &fakeFS{
		files: map[string][]byte{
			"6.5.30/disc1/dist/sa":          content,
			"6.5.30/disc1/dist/PRODUCT.idb": []byte("product descriptor\n"),
		},
		dirs: map[string][]string{
			"6.5.30/disc1/dist": {"sa", "PRODUCT.idb", "miniroot"},
		},
	}
}

// runShell runs a shell script and returns stdout/stderr, logging at
// DEBUG so every command line and any refusal shows up in t.Logf.
func runShell(t *testing.T, script string) (string, string) {
	t.Helper()
	out, errb, _ := runShellWithFS(t, shellTestFS(), logging.LevelDebug, script)
	return out, errb
}

// runShellWithFS is runShell generalized over the FileSystem and log
// level, for a test that needs the log itself - the served-file
// manifest, or the default (non-verbose) level - rather than just
// stdout/stderr.
func runShellWithFS(t *testing.T, fsys FileSystem, level logging.Level, script string) (out, errb, log string) {
	t.Helper()
	var outb, errbb, logbuf strings.Builder
	logger := logging.New(&logbuf, level)
	if err := RunShell(fsys, strings.NewReader(script), &outb, &errbb, logger, nil); err != nil {
		t.Fatalf("RunShell: %v (stderr: %s)", err, errbb.String())
	}
	t.Logf("server log:\n%s", logbuf.String())
	return outb.String(), errbb.String(), logbuf.String()
}

// resolvingFS wraps a *fakeFS with ImageResolver, so a test can check
// the "served ... <- image:path" manifest line dd/cat emit when the
// underlying FileSystem can say where a path's bytes actually live.
type resolvingFS struct {
	*fakeFS
	images map[string]Resolved
}

func (f resolvingFS) ResolveImage(path string) (Resolved, error) {
	r, ok := f.images[path]
	if !ok {
		return Resolved{}, ErrNotFound
	}
	return r, nil
}

// TestShellMarkerProtocol runs inst's real wrapper - a dd, then a
// separate line with trap/subshell/$?/echo '\c' - through the actual
// interpreter, matching what a live Octane2 PROM session sends. The
// wrapped-status suffix "...IsDone0" is deliberate: trap : 2 always
// succeeds under our no-op trap handling, so $? (captured as $status
// inside the subshell) is 0 regardless of what dd did on the previous
// line, exactly as a real shell would compute it here.
func TestShellMarkerProtocol(t *testing.T) {
	script := "dd if=/6.5.30/disc1/dist/sa bs=512 count=1\n" +
		"trap : 2 ; ( status=$? ; trap '' 2 ; echo 'o?_InstProc9IsDone\\c' ; echo 'o?_InstProc9IsDone'$status'\\c' 1>&2 )\n"
	out, errb := runShell(t, script)

	if !strings.HasSuffix(out, "o?_InstProc9IsDone") {
		t.Fatalf("stdout missing trailing marker: ...%q", tail(out, 40))
	}
	if strings.HasSuffix(out, "\n") {
		t.Fatal("marker must not be newline-terminated (\\c)")
	}
	wantDD := shellTestFS().files["6.5.30/disc1/dist/sa"][:512]
	if !bytes.Equal([]byte(out[:512]), wantDD) {
		t.Fatalf("dd output before marker is not byte-exact")
	}

	if !strings.HasSuffix(errb, "o?_InstProc9IsDone0") {
		t.Fatalf("stderr missing trailing marker+status: %q", errb)
	}
	if strings.HasSuffix(errb, "\n") {
		t.Fatal("stderr marker must not be newline-terminated")
	}
}

func TestShellSIGINTStopsTransferAndContinuesInstWrapper(t *testing.T) {
	fsys := shellTestFS()
	fsys.files["6.5.30/disc1/dist/sa"] = bytes.Repeat(fsys.files["6.5.30/disc1/dist/sa"], 64)
	script := "dd if=/6.5.30/disc1/dist/sa bs=512 ; ( status=$? ; trap '' 2 ; echo 'DD_STATUS='$status )\n" +
		"dd if=/6.5.30/disc1/dist/sa bs=512 iseek=1 count=1 ; echo NEXT_STATUS=$?\n" +
		"echo after\n"
	firstBlock, rest := interruptShellTransferWithFS(t, fsys, script, true)
	wantFirst := fsys.files["6.5.30/disc1/dist/sa"][:32*1024]
	if !bytes.Equal(firstBlock, wantFirst) {
		t.Fatal("first block does not match the served file")
	}
	status := []byte("DD_STATUS=130\n")
	statusAt := bytes.Index(rest, status)
	if statusAt < 0 {
		t.Fatalf("interrupted dd status marker missing: %q", rest)
	}
	nextBlock := fsys.files["6.5.30/disc1/dist/sa"][512:1024]
	if !bytes.Equal(rest[statusAt+len(status):statusAt+len(status)+len(nextBlock)], nextBlock) {
		t.Fatalf("next seek output is not byte-exact: %q", rest)
	}
	if !bytes.Contains(rest[statusAt+len(status)+len(nextBlock):], []byte("NEXT_STATUS=0\nafter\n")) {
		t.Fatalf("shell did not finish the next seek and following command: %q", rest)
	}
}

func TestShellSIGINTInterruptsCatAndContinues(t *testing.T) {
	fsys := shellTestFS()
	fsys.files["6.5.30/disc1/dist/sa"] = bytes.Repeat([]byte("x"), 8<<20)
	script := "cat /6.5.30/disc1/dist/sa ; ( status=$? ; echo 'CAT_STATUS='$status )\necho after\n"
	firstBlock, rest := interruptShellTransferWithFS(t, fsys, script, false)
	wantFirst := fsys.files["6.5.30/disc1/dist/sa"][:512]
	if !bytes.Equal(firstBlock, wantFirst) {
		t.Fatal("first cat bytes do not match the served file")
	}
	if !bytes.Contains(rest, []byte("CAT_STATUS=130\nafter\n")) {
		t.Fatalf("interrupted cat did not preserve shell continuation: %q", rest)
	}
}

func TestShellSIGINTPipelineStagesShareInterruption(t *testing.T) {
	fsys := shellTestFS()
	// The interpreter connects pipeline stages with an OS pipe. Keep cat
	// active after dd blocks on the socket so both transfers are in flight.
	fsys.files["6.5.30/disc1/dist/sa"] = bytes.Repeat([]byte("x"), 8<<20)
	script := "cat /6.5.30/disc1/dist/sa | dd bs=512 ; ( status=$? ; echo 'PIPE_STATUS='$status )\necho after\n"
	_, rest := interruptShellTransferWithFS(t, fsys, script, true)
	if !bytes.Contains(rest, []byte("PIPE_STATUS=130\nafter\n")) {
		t.Fatalf("pipeline error suppressed its marker or next command: %q", rest)
	}
}

func TestShellSIGINTUnsupportedPipelineKeepsPriorBehavior(t *testing.T) {
	fsys := shellTestFS()
	fsys.files["6.5.30/disc1/dist/sa"] = bytes.Repeat([]byte("matched\n"), 1024)
	want := fsys.files["6.5.30/disc1/dist/sa"]
	server, client := net.Pipe()
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	stdin, input := io.Pipe()
	writes := make(chan struct{}, 16)
	deadlines := make(chan struct{}, 1)
	stdout := &observedConn{Conn: server, writes: writes, deadlines: deadlines}
	signals := make(chan byte, 1)
	done := make(chan error, 1)
	t.Cleanup(func() {
		input.Close()
		stdin.Close()
		client.Close()
		server.Close()
	})
	go func() {
		done <- RunShellWithSignals(fsys, stdin, stdout, io.Discard, signals, logging.New(io.Discard, logging.LevelDebug), nil)
		server.Close()
	}()
	if _, err := io.WriteString(input, "cat /6.5.30/disc1/dist/sa | grep matched ; echo PIPE_STATUS=$?\necho after\n"); err != nil {
		t.Fatalf("write pipeline command: %v", err)
	}
	select {
	case <-writes:
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline did not begin writing output")
	}
	first := make([]byte, 512)
	if _, err := io.ReadFull(client, first); err != nil {
		t.Fatalf("read first pipeline bytes: %v", err)
	}
	signals <- 2
	select {
	case <-deadlines:
		t.Fatal("unsupported pipeline did not retain prior signal-ignore behavior")
	case <-time.After(50 * time.Millisecond):
	}
	got := make([]byte, len(want))
	copy(got, first)
	if _, err := io.ReadFull(client, got[len(first):]); err != nil {
		t.Fatalf("read remaining pipeline output: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("SIGINT changed the unsupported pipeline output")
	}
	marker := make([]byte, len("PIPE_STATUS=0\nafter\n"))
	if _, err := io.ReadFull(client, marker); err != nil {
		t.Fatalf("read pipeline status marker: %v", err)
	}
	if string(marker) != "PIPE_STATUS=0\nafter\n" {
		t.Fatalf("pipeline status and next command = %q", marker)
	}
	for {
		select {
		case <-writes:
		default:
			goto drained
		}
	}
drained:
	if _, err := io.WriteString(input, "dd if=/6.5.30/disc1/dist/sa bs=512 ; echo DD_STATUS=$?\n"); err != nil {
		t.Fatalf("write following dd command: %v", err)
	}
	select {
	case <-writes:
	case <-time.After(2 * time.Second):
		t.Fatal("following dd did not begin writing")
	}
	signals <- 2
	select {
	case <-deadlines:
	case <-time.After(2 * time.Second):
		t.Fatal("signal handling was not restored for the following dd")
	}
	if err := input.Close(); err != nil {
		t.Fatalf("close shell input: %v", err)
	}
	// A write already in flight can deliver bytes before the deadline takes
	// effect. Drain that prefix before checking the interrupted status.
	rest, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("read interrupted dd output: %v", err)
	}
	ddStatus := []byte("DD_STATUS=130\n")
	if !bytes.HasSuffix(rest, ddStatus) {
		t.Fatalf("following dd output has no interrupted status: %q", rest)
	}
	if output := bytes.TrimSuffix(rest, ddStatus); !bytes.HasPrefix(want, output) {
		t.Fatalf("following dd output is not a file prefix: %q", output)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunShellWithSignals: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shell did not finish after the interrupted dd")
	}
}

func TestUnsupportedTransferPipelineClassifier(t *testing.T) {
	parser := syntax.NewParser(syntax.Variant(syntax.LangPOSIX))
	for _, tc := range []struct {
		line        string
		unsupported bool
	}{
		{line: "cat file | dd bs=512"},
		{line: "dd if=file | dd bs=512"},
		{line: "cat file | grep matched", unsupported: true},
		{line: "(cat file) | grep matched", unsupported: true},
		{line: "$cmd | grep matched", unsupported: true},
		{line: "echo abc | dd", unsupported: true},
	} {
		file, err := parser.Parse(strings.NewReader(tc.line), "")
		if err != nil {
			t.Fatalf("parse %q: %v", tc.line, err)
		}
		if got := hasUnsupportedTransferPipeline(file); got != tc.unsupported {
			t.Errorf("hasUnsupportedTransferPipeline(%q) = %t, want %t", tc.line, got, tc.unsupported)
		}
	}
}

func TestShellBackgroundSyntaxIgnoresSIGINT(t *testing.T) {
	fsys := shellTestFS()
	server, client := net.Pipe()
	conn := &heldTransferConn{
		Conn:        server,
		dataWrite:   make(chan struct{}, 1),
		markerWrite: make(chan struct{}, 2),
		deadlines:   make(chan struct{}, 1),
	}
	stdin, input := io.Pipe()
	signals := make(chan byte, 1)
	done := make(chan error, 1)
	t.Cleanup(func() {
		input.Close()
		stdin.Close()
		client.Close()
		server.Close()
	})
	go func() {
		done <- RunShellWithSignals(fsys, stdin, conn, io.Discard, signals, logging.New(io.Discard, logging.LevelDebug), nil)
	}()
	inputWriterDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(input, "dd if=/6.5.30/disc1/dist/sa bs=512 count=1 &\n")
		inputWriterDone <- err
	}()
	select {
	case <-conn.dataWrite:
	case <-time.After(2 * time.Second):
		t.Fatal("background dd write did not start")
	}
	select {
	case err := <-inputWriterDone:
		if err != nil {
			t.Fatalf("write shell commands: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shell did not read both commands")
	}
	signals <- 2
	select {
	case <-conn.deadlines:
		t.Fatal("SIGINT unexpectedly set a deadline in a background-using session")
	case <-time.After(50 * time.Millisecond):
	}
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 512)
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read background transfer after ignored SIGINT: %v", err)
	}
	if want := fsys.files["6.5.30/disc1/dist/sa"][:512]; !bytes.Equal(got, want) {
		t.Fatal("background transfer output changed after SIGINT")
	}
	if _, err := io.WriteString(input, "echo marker\n"); err != nil {
		t.Fatalf("write foreground marker command: %v", err)
	}
	select {
	case <-conn.markerWrite:
	case <-time.After(2 * time.Second):
		t.Fatal("foreground echo did not run after background transfer")
	}
	got = make([]byte, len("marker\n"))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read foreground marker: %v", err)
	}
	if string(got) != "marker\n" {
		t.Fatalf("foreground output = %q, want %q", got, "marker\n")
	}
	if err := input.Close(); err != nil {
		t.Fatalf("close shell input: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunShellWithSignals: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shell did not finish after the foreground marker")
	}
}

func TestShellBackgroundTransferContinuesAfterInputEOF(t *testing.T) {
	fsys := shellTestFS()
	data := bytes.Repeat([]byte("x"), 8<<10)
	fsys.files["6.5.30/disc1/dist/sa"] = data
	stdout := &blockedFirstWrite{started: make(chan struct{}), release: make(chan struct{}), completed: make(chan struct{}), want: len(data)}
	stdin, input := io.Pipe()
	t.Cleanup(func() { stdin.Close(); input.Close() })
	done := make(chan error, 1)
	go func() {
		done <- RunShellWithSignals(fsys, stdin, stdout, io.Discard, nil, logging.New(io.Discard, logging.LevelDebug), nil)
	}()
	if _, err := io.WriteString(input, "dd if=/6.5.30/disc1/dist/sa bs=512 &\n"); err != nil {
		t.Fatalf("write background command: %v", err)
	}
	select {
	case <-stdout.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background transfer did not start writing")
	}
	if err := input.Close(); err != nil {
		t.Fatalf("close shell input: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunShellWithSignals: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shell waited for a background transfer at input EOF")
	}
	close(stdout.release)
	select {
	case <-stdout.completed:
	case <-time.After(2 * time.Second):
		t.Fatal("background transfer did not complete after shell EOF")
	}
}

func TestTransferInterruptControllerShutdownDoesNotStrandTransfers(t *testing.T) {
	signals := make(chan byte, 1)
	deadlines := make(chan time.Time, 2)
	controller := newTransferInterruptController(signals, deadlineRecorder{deadlines: deadlines})
	transfer := controller.begin()
	signals <- 2
	select {
	case deadline := <-deadlines:
		if deadline.IsZero() {
			t.Fatal("SIGINT did not install an output deadline")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SIGINT did not reach the active transfer")
	}
	controller.close()
	select {
	case deadline := <-deadlines:
		if deadline.IsZero() {
			t.Fatal("controller cleared the output deadline before its active transfer finished")
		}
	default:
	}

	finished := make(chan struct{})
	go func() {
		transfer.finish()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("active transfer finish blocked after controller shutdown")
	}
	select {
	case deadline := <-deadlines:
		if !deadline.IsZero() {
			t.Fatal("transfer finish did not clear its output deadline")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("transfer finish did not restore the output deadline")
	}
	late := make(chan *fileTransferInterrupt, 1)
	go func() { late <- controller.begin() }()
	select {
	case transfer := <-late:
		transfer.finish()
	case <-time.After(2 * time.Second):
		t.Fatal("transfer started after shutdown blocked on the stopped controller")
	}
}

func interruptShellTransferWithFS(t *testing.T, fsys FileSystem, script string, waitForSecondWrite bool) ([]byte, []byte) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { client.Close() })
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	writes := make(chan struct{}, 16)
	deadlines := make(chan struct{}, 1)
	stdout := &observedConn{Conn: server, writes: writes, deadlines: deadlines}
	signals := make(chan byte, 1)
	done := make(chan error, 1)
	go func() {
		var errbuf strings.Builder
		logger := logging.New(io.Discard, logging.LevelDebug)
		done <- RunShellWithSignals(fsys, strings.NewReader(script), stdout, &errbuf, signals, logger, nil)
		server.Close()
	}()

	select {
	case <-writes:
	case <-time.After(2 * time.Second):
		t.Fatal("file transfer did not begin writing")
	}
	firstSize := 512
	if waitForSecondWrite {
		// Consume one complete group so the next grouped write can block.
		firstSize = 32 * 1024
	}
	firstBlock := make([]byte, firstSize)
	if _, err := io.ReadFull(client, firstBlock); err != nil {
		t.Fatalf("read first block: %v", err)
	}
	if waitForSecondWrite {
		select {
		case <-writes:
		case <-time.After(2 * time.Second):
			t.Fatal("transfer did not start the blocked next write")
		}
	}
	signals <- 2
	select {
	case <-deadlines:
	case <-time.After(2 * time.Second):
		t.Fatal("SIGINT did not interrupt the blocked socket write")
	}

	rest, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("bounded read of wrapper output: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunShellWithSignals: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shell did not finish after interrupted transfer")
	}
	return firstBlock, rest
}

type blockedFirstWrite struct {
	started   chan struct{}
	release   chan struct{}
	completed chan struct{}
	want      int
	mu        sync.Mutex
	written   int
	first     sync.Once
	done      sync.Once
}

func (w *blockedFirstWrite) Write(p []byte) (int, error) {
	w.first.Do(func() {
		close(w.started)
		<-w.release
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	w.written += len(p)
	if w.written >= w.want {
		w.done.Do(func() { close(w.completed) })
	}
	return len(p), nil
}

type deadlineRecorder struct {
	deadlines chan<- time.Time
}

type heldTransferConn struct {
	net.Conn
	dataWrite   chan struct{}
	markerWrite chan struct{}
	deadlines   chan struct{}
}

func (c *heldTransferConn) Write(p []byte) (int, error) {
	data := len(p) == 512
	marker := bytes.Equal(p, []byte("marker\n"))
	if data {
		c.dataWrite <- struct{}{}
	}
	if marker {
		c.markerWrite <- struct{}{}
	}
	n, err := c.Conn.Write(p)
	return n, err
}

func (c *heldTransferConn) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		select {
		case c.deadlines <- struct{}{}:
		default:
		}
	}
	return c.Conn.SetWriteDeadline(deadline)
}

func (d deadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	d.deadlines <- deadline
	return nil
}

func TestShellIgnoresIdleAndNonInterruptSignals(t *testing.T) {
	var out, errout strings.Builder
	signals := make(chan byte, 2)
	signals <- 2
	signals <- 1
	script := "dd if=/6.5.30/disc1/dist/sa bs=512 count=1 ; ( status=$? ; echo 'DD_STATUS='$status )\n"
	logger := logging.New(io.Discard, logging.LevelDebug)
	if err := RunShellWithSignals(shellTestFS(), strings.NewReader(script), &out, &errout, signals, logger, nil); err != nil {
		t.Fatalf("RunShellWithSignals: %v (stderr: %s)", err, errout.String())
	}
	if !strings.HasSuffix(out.String(), "DD_STATUS=0\n") {
		t.Fatalf("idle SIGINT or unsupported signal interrupted the transfer: %q", out.String())
	}
	if len(out.String()) != 512+len("DD_STATUS=0\n") {
		t.Fatalf("transfer output length = %d, want one block plus marker", len(out.String()))
	}
}

type observedConn struct {
	net.Conn
	writes    chan<- struct{}
	deadlines chan<- struct{}
}

func (c *observedConn) Write(p []byte) (int, error) {
	select {
	case c.writes <- struct{}{}:
	default:
	}
	return c.Conn.Write(p)
}

func (c *observedConn) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		select {
		case c.deadlines <- struct{}{}:
		default:
		}
	}
	return c.Conn.SetWriteDeadline(deadline)
}

// TestShellPipeIntoDD covers inst's capability probe: a pipe feeding
// dd's stdin.
func TestShellPipeIntoDD(t *testing.T) {
	out, errb := runShell(t, "echo abc|dd iseek=0\n")
	if out != "abc\n" {
		t.Fatalf("piped dd output = %q, want %q", out, "abc\n")
	}
	if !strings.Contains(errb, "0+1 records in") || !strings.Contains(errb, "0+1 records out") {
		t.Fatalf("dd records summary missing: %q", errb)
	}
}

// TestShellDDReadsVFSByteExact drives dd's normal if=<path> mode with a
// block size that doesn't divide the file evenly, over a non-uniform
// byte pattern, so truncation, off-by-one, or block reordering would
// change the hash instead of silently passing.
func TestShellDDReadsVFSByteExact(t *testing.T) {
	out, _ := runShell(t, "dd if=/6.5.30/disc1/dist/sa bs=7\n")
	want := shellTestFS().files["6.5.30/disc1/dist/sa"]
	if len(out) != len(want) {
		t.Fatalf("dd copied %d bytes, want %d", len(out), len(want))
	}
	if !bytes.Equal([]byte(out), want) {
		t.Fatal("dd output is not byte-exact")
	}
}

// TestShellDDLogsServedFileWithImage checks that at the default
// (non-verbose) level, dd logs the served path and its backing image,
// not the raw "dd if=..." command line - that trace stays behind -v.
func TestShellDDLogsServedFileWithImage(t *testing.T) {
	fs := resolvingFS{
		fakeFS: shellTestFS(),
		images: map[string]Resolved{
			"6.5.30/disc1/dist/sa": {Image: "Overlay 1of3.iso", Path: "dist/sa"},
		},
	}
	_, _, log := runShellWithFS(t, fs, logging.LevelInfo, "dd if=/6.5.30/disc1/dist/sa bs=512 count=1\n")
	want := "instcmd: served /6.5.30/disc1/dist/sa  <-  Overlay 1of3.iso:/dist/sa"
	if !strings.Contains(log, want) {
		t.Fatalf("log missing served line %q:\n%s", want, log)
	}
	if strings.Contains(log, "rsh-sh:") {
		t.Fatalf("raw command trace leaked into the default-level log:\n%s", log)
	}
}

// TestShellDDLogsServedFileWithoutImageResolver checks the fallback: a
// FileSystem that can't say which image a path came from (fakeFS,
// here) still gets the served-path line, just without the mapping.
func TestShellDDLogsServedFileWithoutImageResolver(t *testing.T) {
	_, _, log := runShellWithFS(t, shellTestFS(), logging.LevelInfo, "dd if=/6.5.30/disc1/dist/sa bs=512 count=1\n")
	want := "instcmd: served /6.5.30/disc1/dist/sa"
	if !strings.Contains(log, want) {
		t.Fatalf("log missing served line %q:\n%s", want, log)
	}
	if strings.Contains(log, "<-") {
		t.Fatalf("served line has an image mapping with no ImageResolver available:\n%s", log)
	}
}

// TestShellCatLogsServedFile checks cat gets the same manifest
// treatment as dd - it is the other leaf command that actually serves
// a file's content.
func TestShellCatLogsServedFile(t *testing.T) {
	_, _, log := runShellWithFS(t, shellTestFS(), logging.LevelInfo, "cat /6.5.30/disc1/dist/PRODUCT.idb\n")
	want := "instcmd: served /6.5.30/disc1/dist/PRODUCT.idb"
	if !strings.Contains(log, want) {
		t.Fatalf("log missing served line %q:\n%s", want, log)
	}
}

// TestShellUnknownCommandRefused checks that a command outside the
// whitelist fails with a diagnostic and a nonzero exit, and the shell
// keeps going afterward, as a real shell does for a ';'-separated list.
func TestShellUnknownCommandRefused(t *testing.T) {
	out, errb := runShell(t, "rm -rf /\necho survived\n")
	if !strings.Contains(out, "survived") {
		t.Fatalf("shell did not continue past refused command: out=%q err=%q", out, errb)
	}
	if !strings.Contains(errb, "not supported") {
		t.Fatalf("refusal not reported to stderr: %q", errb)
	}
}

// TestShellTestBuiltin exercises test -f/-d, which inst is expected to
// send wrapped in the marker. mvdan/sh's own test/[ builtin evaluates
// these via our vfs-backed StatHandler.
func TestShellTestBuiltin(t *testing.T) {
	script := "test -f /6.5.30/disc1/dist/sa && echo FILE_YES\n" +
		"test -d /6.5.30/disc1/dist && echo DIR_YES\n" +
		"test -f /absent && echo SHOULD_NOT_APPEAR\n" +
		"[ -f /6.5.30/disc1/dist/PRODUCT.idb ] && echo BRACKET_YES\n"
	out, _ := runShell(t, script)
	for _, want := range []string{"FILE_YES", "DIR_YES", "BRACKET_YES"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output: %q", want, out)
		}
	}
	if strings.Contains(out, "SHOULD_NOT_APPEAR") {
		t.Fatalf("test -f on a missing file reported success: %q", out)
	}
}

// TestShellNeverReachesHostFilesystem is the non-negotiable security
// check: no path, redirect, or test operator can read, write, or probe
// anything on the real host filesystem. /etc/passwd exists on the host
// but not in the vfs, so every one of these must fail exactly the way an
// absent vfs path fails - never by actually touching the host file.
func TestShellNeverReachesHostFilesystem(t *testing.T) {
	script := "cat /etc/passwd\n" +
		"echo leak > /etc/passwd\n" +
		"echo REDIRECT_EXIT=$?\n" +
		"cd /etc\n" +
		"test -r /etc/passwd && echo READABLE\n" +
		"test -w /etc/passwd && echo WRITABLE\n" +
		"test -x /etc/passwd && echo EXECUTABLE\n" +
		"echo done\n"
	out, errb := runShell(t, script)

	if strings.Contains(out, "root") || strings.Contains(out, ":0:0:") {
		t.Fatalf("host /etc/passwd content leaked into stdout: %q", out)
	}
	if !strings.Contains(out, "REDIRECT_EXIT=1") {
		t.Fatalf("write redirect to a host path did not fail with a nonzero exit: %q", out)
	}
	for _, forbidden := range []string{"READABLE", "WRITABLE", "EXECUTABLE"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("test operator reached the real host filesystem: got %q in %q", forbidden, out)
		}
	}
	if !strings.Contains(out, "done") {
		t.Fatalf("shell did not survive the refused commands: out=%q err=%q", out, errb)
	}
	if !strings.Contains(errb, "not supported") && !strings.Contains(errb, "no such") && !strings.Contains(errb, "not found") {
		t.Fatalf("no refusal/not-found diagnostic seen on stderr: %q", errb)
	}
}

// TestShellEchoBackslashC is a focused check on the exact bug the
// mvdan/sh library has: its builtin echo has no \c support at all, even
// with -e, so plain echo 'TOKEN\c' would print the literal characters
// "TOKEN\c" plus a trailing newline instead of "TOKEN" with none.
func TestShellEchoBackslashC(t *testing.T) {
	out, _ := runShell(t, "echo 'TOKEN\\c'\n")
	if out != "TOKEN" {
		t.Fatalf("echo with \\c = %q, want %q (no trailing newline, no literal \\c)", out, "TOKEN")
	}

	out2, _ := runShell(t, "echo hello world\n")
	if out2 != "hello world\n" {
		t.Fatalf("plain echo = %q, want %q", out2, "hello world\n")
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// assertLsLongLine checks the shape inst actually parses out of a long
// ls line: a leading numeric inode field and a permission string
// starting with the directory type bit.
func assertLsLongLine(t *testing.T, line string) {
	t.Helper()
	fields := strings.Fields(line)
	if len(fields) < 8 {
		t.Fatalf("long ls line has too few fields (%d): %q", len(fields), line)
	}
	if _, err := strconv.ParseUint(fields[0], 10, 64); err != nil {
		t.Fatalf("inode field %q is not a number: %v (line %q)", fields[0], err, line)
	}
	if fields[1] == "" || fields[1][0] != 'd' {
		t.Fatalf("permission field %q doesn't start with 'd': %q", fields[1], line)
	}
}

// TestShellLsProbeSequence runs the exact three probes a live Octane2
// session sends at the start of a shell, in order: inst uses these to
// stat "." for its inode/identity before it ever cds anywhere.
func TestShellLsProbeSequence(t *testing.T) {
	script := "ls -inld .\n" +
		"/usr/5bin/ls -inld .\n" +
		"ls -inlgd .\n"
	out, errb := runShell(t, script)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d ls lines, want 3: out=%q stderr=%q", len(lines), out, errb)
	}
	for _, line := range lines {
		assertLsLongLine(t, line)
	}
	if strings.Contains(errb, "not supported") || strings.Contains(errb, "not found") {
		t.Fatalf("a probe was refused instead of answered: stderr=%q", errb)
	}
}

// TestShellCdAndRelativeDD is the flow after the probes succeed: inst
// cds into the distribution, then reads a file by a bare relative name.
func TestShellCdAndRelativeDD(t *testing.T) {
	out, errb := runShell(t, "cd /6.5.30/disc1/dist\ndd if=sa bs=7\n")
	want := shellTestFS().files["6.5.30/disc1/dist/sa"]
	if len(out) != len(want) {
		t.Fatalf("relative dd after cd copied %d bytes, want %d (stderr %q)", len(out), len(want), errb)
	}
	if !bytes.Equal([]byte(out), want) {
		t.Fatal("relative dd after cd is not byte-exact")
	}
}

// TestShellLsLongFormatAfterCd checks ls -inld . reports the new
// directory once cwd has actually moved, not the one the shell started
// in.
func TestShellLsLongFormatAfterCd(t *testing.T) {
	out, errb := runShell(t, "cd /6.5.30/disc1/dist\nls -inld .\n")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d ls lines, want 1: out=%q stderr=%q", len(lines), out, errb)
	}
	assertLsLongLine(t, lines[0])
}

// TestShellCdRefusesPathOutsideVFS is the security case for this layer:
// cd only ever resolves through the vfs, so a path that doesn't exist
// there - including a real host path like /etc - fails and leaves cwd
// unchanged, proven here by a relative dd that would only succeed if cd
// had silently moved somewhere it shouldn't have.
func TestShellCdRefusesPathOutsideVFS(t *testing.T) {
	out, errb := runShell(t, "cd /etc\necho CDSTATUS=$?\ndd if=sa bs=7\necho DDSTATUS=$?\n")
	if !strings.Contains(out, "CDSTATUS=1") {
		t.Fatalf("cd to a path outside the vfs did not fail: out=%q stderr=%q", out, errb)
	}
	if !strings.Contains(out, "DDSTATUS=1") {
		t.Fatalf("cwd changed despite the failed cd: relative dd found a file anyway: out=%q stderr=%q", out, errb)
	}
}

// TestShellLsPlainStaysNameOnly guards the pre-existing behavior: ls
// without -l lists bare names, cwd-relative dirs included.
func TestShellLsPlainStaysNameOnly(t *testing.T) {
	out, errb := runShell(t, "cd /6.5.30/disc1/dist\nls .\n")
	if err := errb; err != "" && strings.Contains(err, "not supported") {
		t.Fatalf("plain ls refused: %q", errb)
	}
	for _, want := range []string{"sa", "PRODUCT.idb", "miniroot"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plain ls missing %q: %q", want, out)
		}
	}
	if strings.ContainsAny(out, "0123456789") {
		t.Fatalf("plain ls (no -l) printed something numeric, expected bare names only: %q", out)
	}
}

// logLineRE matches one leveled log line: an ISO8601 timestamp, a
// level word, then the message - the same shape internal/logging's own
// tests check.
var logLineRE = regexp.MustCompile(`(?m)^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:Z|[+-]\d{2}:\d{2}) +(DEBUG|INFO|WARN|ERROR) +(.*)$`)

// A refused command must reach both client stderr and the default-level
// server log.
func TestShellRefusedCommandLogsErrorOnServerSide(t *testing.T) {
	var out, errb, logbuf strings.Builder
	logger := logging.New(&logbuf, logging.LevelInfo) // default level: no -v
	script := "not-a-real-command foo\n"
	if err := RunShell(shellTestFS(), strings.NewReader(script), &out, &errb, logger, nil); err != nil {
		t.Fatal(err)
	}

	// the client still sees the refusal on its own stderr, unchanged
	if !strings.Contains(errb.String(), "not supported") {
		t.Fatalf("client stderr missing the refusal: %q", errb.String())
	}

	// and now it also reached our own log, as an ERROR line in the
	// <ISO8601> <LEVEL> <message> shape
	var found string
	for _, m := range logLineRE.FindAllStringSubmatch(logbuf.String(), -1) {
		if m[1] == "ERROR" && strings.Contains(m[2], "not-a-real-command") {
			found = m[0]
			break
		}
	}
	if found == "" {
		t.Fatalf("no ERROR line for the refused command in the server log:\n%s", logbuf.String())
	}
	if !strings.Contains(found, "not supported") {
		t.Fatalf("ERROR line missing %q: %q", "not supported", found)
	}
}
