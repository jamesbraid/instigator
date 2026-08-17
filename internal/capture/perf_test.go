package capture

import (
	"io"
	"strings"
	"testing"
)

func TestCommandSplitsStdoutAndStderr(t *testing.T) {
	r, dir := newTestRecorder(t)
	s := r.BeginSession("octane", "a", "g", "g")
	cmd := s.BeginCommand("dd if=/dist/sa bs=512")
	out := s.WrapWriter(io.Discard, false) // stdout
	errw := s.WrapWriter(io.Discard, true) // stderr
	out.Write(make([]byte, 512))           // command data
	errw.Write(make([]byte, 30))           // dd's records summary
	cmd.End(0, false)
	r.Close()

	e := only(t, dir, "inst_command_end")
	if e["stdout_bytes"] != float64(512) {
		t.Errorf("stdout_bytes = %v, want 512", e["stdout_bytes"])
	}
	if e["stderr_bytes"] != float64(30) {
		t.Errorf("stderr_bytes = %v, want 30", e["stderr_bytes"])
	}
}

// Throughput must be non-marker stdout bytes over time, matching its
// label - not session bytes_out, which folds in stderr and marker output
// while active time excludes marker time.
func TestSummarizeThroughputUsesStdoutNotBytesOut(t *testing.T) {
	in := jsonl(
		map[string]any{"event": "rsh_session_start", "session": "s1"},
		map[string]any{"event": "inst_command_end", "session": "s1", "seq": 1, "verb": "dd",
			"duration_ms": 500, "result": "ok", "stdout_bytes": 1000, "stderr_bytes": 40},
		map[string]any{"event": "inst_command_end", "session": "s1", "seq": 2, "verb": "trap",
			"duration_ms": 100, "result": "ok", "stdout_bytes": 30, "marker": true},
		// bytes_out (2000) folds in stderr and marker output; throughput must ignore it
		map[string]any{"event": "rsh_session_end", "session": "s1", "result": "ok", "duration_ms": 1000, "bytes_out": 2000},
	)
	sum, err := Summarize(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	s := sum.Sessions[0]
	if s.ActiveBps != 2000 {
		t.Errorf("active throughput = %d B/s, want 2000 (1000 stdout B over 0.5s active)", s.ActiveBps)
	}
	if s.WallBps != 1000 {
		t.Errorf("wall throughput = %d B/s, want 1000 (1000 stdout B over 1s wall)", s.WallBps)
	}
}
