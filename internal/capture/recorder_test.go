package capture

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// readLines returns the non-empty lines of the capture's events.jsonl.
func readLines(t *testing.T, dir string) []string {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("open events.jsonl: %v", err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			lines = append(lines, sc.Text())
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan events.jsonl: %v", err)
	}
	return lines
}

func TestNewCreatesPrivateDirAndEventsFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	r, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("capture dir perm = %o, want 700", perm)
	}
	fi, err := os.Stat(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("stat events.jsonl: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("events.jsonl perm = %o, want 600", perm)
	}
}

func TestEmitWritesOneEnvelopeLinePerEvent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	r, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.ServerStart()
	r.ServerStop("clean")
	r.Close()

	lines := readLines(t, dir)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), lines)
	}

	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 0 not valid JSON: %v", err)
	}
	for _, k := range []string{"v", "ts", "run", "event"} {
		if _, ok := first[k]; !ok {
			t.Errorf("line 0 missing envelope field %q: %v", k, first)
		}
	}
	if first["event"] != "server_start" {
		t.Errorf("line 0 event = %v, want server_start", first["event"])
	}

	var second map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("line 1 not valid JSON: %v", err)
	}
	if second["event"] != "server_stop" {
		t.Errorf("line 1 event = %v, want server_stop", second["event"])
	}
	// run id is stable within a process
	if first["run"] != second["run"] {
		t.Errorf("run id changed between events: %v != %v", first["run"], second["run"])
	}
}

func TestNilRecorderNoOps(t *testing.T) {
	var r *Recorder // nil
	// none of these may panic
	r.ServerStart()
	r.ServerStop("clean")
	if err := r.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}
}

func TestConcurrentEmitsAreAllWellFormed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	r, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.ServerStart()
		}()
	}
	wg.Wait()
	r.Close()

	lines := readLines(t, dir)
	if len(lines) != n {
		t.Fatalf("got %d lines, want %d", len(lines), n)
	}
	for i, ln := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("line %d not valid JSON (interleaved write?): %v: %q", i, err, ln)
		}
	}
}
