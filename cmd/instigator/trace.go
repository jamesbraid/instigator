package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/jamesbraid/instigator/internal/capture"
)

// runTraceSummary reads a capture's events.jsonl, writes the human summary
// to w, and regenerates summary.json in the capture directory - the same
// aggregation the server runs at a clean shutdown, available offline on any
// captured run.
func runTraceSummary(w io.Writer, dir string) error {
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return err
	}
	defer f.Close()

	sum, err := capture.Summarize(f)
	if err != nil {
		return err
	}
	sum.WriteText(w)

	b, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "summary.json"), append(b, '\n'), 0o600)
}
