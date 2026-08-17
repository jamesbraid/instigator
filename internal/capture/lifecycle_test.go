package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListenerExitEvent(t *testing.T) {
	r, dir := newTestRecorder(t)
	r.ListenerExit("tftp", true, "closed")
	r.Close()
	e := only(t, dir, "listener_exit")
	if e["listener"] != "tftp" || e["unexpected"] != true {
		t.Errorf("listener_exit = %v", e)
	}
}

func TestFinishWritesSummaryStopAndRunEnd(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	var sink strings.Builder
	r, err := New(dir, WithSummaryWriter(&sink))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.WriteRun(Provenance{Start: "2026-08-16T00:00:00Z", Binary: BuildInfo()}); err != nil {
		t.Fatalf("WriteRun: %v", err)
	}
	s := r.BeginSession("octane", "a", "g", "g")
	s.BeginCommand("dd if=/x bs=512").End(0, false)
	s.End(nil)

	if err := r.Finish("clean"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	// server_stop landed in events.jsonl
	if _, err := os.Stat(filepath.Join(dir, "events.jsonl")); err != nil {
		t.Fatal(err)
	}
	foundStop := false
	for _, ln := range readLines(t, dir) {
		var m map[string]any
		json.Unmarshal([]byte(ln), &m)
		if m["event"] == "server_stop" {
			foundStop = true
		}
	}
	if !foundStop {
		t.Error("no server_stop event after Finish")
	}

	// summary.json written, 0600, with the command counted
	fi, err := os.Stat(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatalf("stat summary.json: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("summary.json perm = %o, want 600", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "summary.json"))
	var sum Summary
	if err := json.Unmarshal(b, &sum); err != nil {
		t.Fatalf("summary.json invalid: %v", err)
	}
	if sum.Commands != 1 {
		t.Errorf("summary commands = %d, want 1", sum.Commands)
	}

	// run.json now carries an end time
	rb, _ := os.ReadFile(filepath.Join(dir, "run.json"))
	var run map[string]any
	json.Unmarshal(rb, &run)
	if run["end"] == nil || run["end"] == "" {
		t.Errorf("run.json end not set after Finish: %v", run["end"])
	}

	// the human summary was rendered to the sink
	if sink.Len() == 0 {
		t.Error("Finish rendered no human summary to the sink")
	}
}
