package capture

import (
	"path/filepath"
	"testing"
)

// A capture directory that already holds a run must not be reused: a
// second run would append its events to the first, mixing two run ids in
// one events.jsonl.
func TestNewRejectsAReusedDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	r, err := New(dir)
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	r.Close()

	if _, err := New(dir); err == nil {
		t.Fatal("New reused a directory that already has a run; want an error")
	}
}

// A write that fails mid-run must not be swallowed: emit records the
// first write error, and Close surfaces it so the operator knows the
// capture is incomplete.
func TestEmitCapturesAStickyWriteError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	r, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Close the underlying file out from under the recorder, then emit:
	// the write now fails and must be recorded, not discarded.
	r.events.Close()
	r.ServerStart()

	if r.writeErr == nil {
		t.Fatal("emit discarded the failed write instead of recording it")
	}
	if err := r.Close(); err == nil {
		t.Fatal("Close did not surface the sticky write error")
	}
}
