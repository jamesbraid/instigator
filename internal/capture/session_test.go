package capture

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// events decodes every event line into a generic map.
func events(t *testing.T, dir string) []map[string]any {
	t.Helper()
	var evs []map[string]any
	for _, ln := range readLines(t, dir) {
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("bad event line %q: %v", ln, err)
		}
		evs = append(evs, m)
	}
	return evs
}

// only returns the single event with the given name, failing otherwise.
func only(t *testing.T, dir, name string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, e := range events(t, dir) {
		if e["event"] == name {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly 1 %q event, got %d", name, len(found))
	}
	return found[0]
}

func newTestRecorder(t *testing.T) (*Recorder, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cap")
	r, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r, dir
}

func TestSessionEmitsStartAndEnd(t *testing.T) {
	r, dir := newTestRecorder(t)
	s := r.BeginSession("octane", "192.0.2.10:1023", "guest", "guest")
	s.End(nil)
	r.Close()

	start := only(t, dir, "rsh_session_start")
	if start["client"] != "octane" {
		t.Errorf("session_start client = %v, want octane", start["client"])
	}
	if start["session"] == nil || start["session"] == "" {
		t.Error("session_start missing session id")
	}
	if start["remote_addr"] != "192.0.2.10:1023" {
		t.Errorf("remote_addr = %v", start["remote_addr"])
	}

	end := only(t, dir, "rsh_session_end")
	if end["session"] != start["session"] {
		t.Errorf("session id mismatch: %v vs %v", end["session"], start["session"])
	}
	if end["result"] != "ok" {
		t.Errorf("clean session end result = %v, want ok", end["result"])
	}
	if _, ok := end["duration_ms"]; !ok {
		t.Error("session_end missing duration_ms")
	}
}

func TestCommandRecordsCountersServedAndVerb(t *testing.T) {
	r, dir := newTestRecorder(t)
	s := r.BeginSession("octane", "192.0.2.10:1023", "guest", "guest")

	cmd := s.BeginCommand("dd if=/dist/sa bs=512")
	// simulate a leaf command reading from EFS and writing to the socket
	backing := bytes.NewReader(make([]byte, 4096))
	rd := s.WrapReaderAt(backing)
	buf := make([]byte, 1024)
	rd.ReadAt(buf, 0)
	rd.ReadAt(buf, 1024)
	w := s.WrapWriter(io.Discard, false)
	w.Write(make([]byte, 512))
	w.Write(make([]byte, 512))
	s.RecordServed("/dist/sa", "foundation1.iso", "dist/sa")
	cmd.End(0, false)

	s.End(nil)
	r.Close()

	end := only(t, dir, "inst_command_end")
	if end["verb"] != "dd" {
		t.Errorf("verb = %v, want dd", end["verb"])
	}
	if end["result"] != "ok" {
		t.Errorf("result = %v, want ok", end["result"])
	}
	if end["exit_status"] != float64(0) {
		t.Errorf("exit_status = %v, want 0", end["exit_status"])
	}
	if end["efs_read_calls"] != float64(2) {
		t.Errorf("efs_read_calls = %v, want 2", end["efs_read_calls"])
	}
	if end["efs_read_bytes"] != float64(2048) {
		t.Errorf("efs_read_bytes = %v, want 2048", end["efs_read_bytes"])
	}
	if end["stdout_calls"] != float64(2) {
		t.Errorf("stdout_calls = %v, want 2", end["stdout_calls"])
	}
	if end["stdout_bytes"] != float64(1024) {
		t.Errorf("stdout_bytes = %v, want 1024", end["stdout_bytes"])
	}
	served, ok := end["served"].([]any)
	if !ok || len(served) != 1 {
		t.Fatalf("served = %v, want 1 entry", end["served"])
	}
	sv0 := served[0].(map[string]any)
	if sv0["image"] != "foundation1.iso" || sv0["image_path"] != "dist/sa" {
		t.Errorf("served[0] = %v", sv0)
	}

	// session_end aggregates the socket bytes it pushed
	sEnd := only(t, dir, "rsh_session_end")
	if sEnd["bytes_out"] != float64(1024) {
		t.Errorf("session bytes_out = %v, want 1024", sEnd["bytes_out"])
	}
	if sEnd["command_count"] != float64(1) {
		t.Errorf("command_count = %v, want 1", sEnd["command_count"])
	}
}

func TestCommandResultRefusedAndNonzero(t *testing.T) {
	r, dir := newTestRecorder(t)
	s := r.BeginSession("octane", "a", "g", "g")
	s.BeginCommand("frobnicate x").End(1, true)
	s.BeginCommand("dd if=/nope bs=512").End(2, false)
	r.Close()

	var results []any
	for _, e := range events(t, dir) {
		if e["event"] == "inst_command_end" {
			results = append(results, e["result"])
		}
	}
	if len(results) != 2 || results[0] != "refused" || results[1] != "nonzero" {
		t.Errorf("command results = %v, want [refused nonzero]", results)
	}
}

func TestMarkerLineFlagged(t *testing.T) {
	r, dir := newTestRecorder(t)
	s := r.BeginSession("octane", "a", "g", "g")
	marker := "trap : 2 ; ( status=$? ; trap '' 2 ; echo 'o?_InstProc9IsDone\\c' ; echo 'o?_InstProc9IsDone'$status'\\c' 1>&2 )"
	s.BeginCommand(marker).End(0, false)
	s.BeginCommand("dd if=/dist/sa bs=512").End(0, false)
	r.Close()

	var markers []bool
	for _, e := range events(t, dir) {
		if e["event"] == "inst_command_end" {
			markers = append(markers, e["marker"] == true)
		}
	}
	if len(markers) != 2 || !markers[0] || markers[1] {
		t.Errorf("marker flags = %v, want [true false]", markers)
	}
}

func TestConcurrentCounterIncrementsAreExact(t *testing.T) {
	r, dir := newTestRecorder(t)
	s := r.BeginSession("octane", "a", "g", "g")
	cmd := s.BeginCommand("cat /dist/idb | fgrep foo")
	w := s.WrapWriter(io.Discard, false)

	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Write(make([]byte, 16))
		}()
	}
	wg.Wait()
	cmd.End(0, false)
	r.Close()

	end := only(t, dir, "inst_command_end")
	if end["stdout_calls"] != float64(n) {
		t.Errorf("stdout_calls = %v, want %d", end["stdout_calls"], n)
	}
	if end["stdout_bytes"] != float64(n*16) {
		t.Errorf("stdout_bytes = %v, want %d", end["stdout_bytes"], n*16)
	}
}

func TestSessionEndClassifiesIdleTimeout(t *testing.T) {
	r, dir := newTestRecorder(t)
	s := r.BeginSession("octane", "a", "g", "g")
	s.End(os.ErrDeadlineExceeded)
	r.Close()

	end := only(t, dir, "rsh_session_end")
	if end["result"] != "idle" {
		t.Errorf("deadline-exceeded session end result = %v, want idle", end["result"])
	}
}
