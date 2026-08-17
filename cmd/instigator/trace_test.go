package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamesbraid/instigator/internal/capture"
)

func TestTraceSummaryReadsCaptureAndRegeneratesJSON(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	rec, err := capture.New(dir)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	s := rec.BeginSession("octane", "a", "g", "g")
	s.BeginCommand("dd if=/dist/sa bs=512").End(0, false)
	s.BeginCommand("rm -rf /").End(1, true)
	s.End(nil)
	rec.Close() // no summary.json yet - Close does not summarize

	if _, err := os.Stat(filepath.Join(dir, "summary.json")); err == nil {
		t.Fatal("summary.json should not exist before trace summary runs")
	}

	var out strings.Builder
	if err := runTraceSummary(&out, dir); err != nil {
		t.Fatalf("runTraceSummary: %v", err)
	}

	if !strings.Contains(out.String(), "refused commands:    1") {
		t.Errorf("summary text missing refused count:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "summary.json")); err != nil {
		t.Errorf("summary.json not regenerated: %v", err)
	}
}
