package instcmd

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamesbraid/instigator/internal/capture"
	"github.com/jamesbraid/instigator/internal/logging"
)

// captureEvents runs a script through RunShell with capture enabled and
// returns the decoded inst_command_end events.
func captureEnds(t *testing.T, fsys FileSystem, script string) []map[string]any {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cap")
	rec, err := capture.New(dir)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	sess := rec.BeginSession("octane", "192.0.2.10:1023", "guest", "guest")
	var out, errb strings.Builder
	logger := logging.New(&out, logging.LevelInfo)
	if err := RunShell(fsys, strings.NewReader(script), &out, &errb, logger, sess); err != nil {
		t.Fatalf("RunShell: %v", err)
	}
	sess.End(nil)
	rec.Close()

	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("open events.jsonl: %v", err)
	}
	defer f.Close()
	var ends []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad event: %v", err)
		}
		if m["event"] == "inst_command_end" {
			ends = append(ends, m)
		}
	}
	return ends
}

func TestRunShellRecordsCatCounters(t *testing.T) {
	fs := resolvingFS{
		fakeFS: shellTestFS(),
		images: map[string]Resolved{
			"6.5.30/disc1/dist/PRODUCT.idb": {Image: "Overlay 1of3.iso", Path: "dist/PRODUCT.idb"},
		},
	}
	content := shellTestFS().files["6.5.30/disc1/dist/PRODUCT.idb"]
	ends := captureEnds(t, fs, "cat /6.5.30/disc1/dist/PRODUCT.idb\n")
	if len(ends) != 1 {
		t.Fatalf("got %d command-end events, want 1", len(ends))
	}
	e := ends[0]
	if e["verb"] != "cat" {
		t.Errorf("verb = %v, want cat", e["verb"])
	}
	if e["result"] != "ok" {
		t.Errorf("result = %v, want ok", e["result"])
	}
	if e["efs_read_bytes"] != float64(len(content)) {
		t.Errorf("efs_read_bytes = %v, want %d", e["efs_read_bytes"], len(content))
	}
	if e["stdout_bytes"] != float64(len(content)) {
		t.Errorf("stdout_bytes = %v, want %d", e["stdout_bytes"], len(content))
	}
	served, ok := e["served"].([]any)
	if !ok || len(served) != 1 {
		t.Fatalf("served = %v, want 1 entry", e["served"])
	}
	if served[0].(map[string]any)["image"] != "Overlay 1of3.iso" {
		t.Errorf("served image = %v", served[0])
	}
}

func TestRunShellRecordsRefusedCommand(t *testing.T) {
	ends := captureEnds(t, shellTestFS(), "rm -rf /\n")
	if len(ends) != 1 {
		t.Fatalf("got %d command-end events, want 1", len(ends))
	}
	if ends[0]["result"] != "refused" {
		t.Errorf("unknown-command result = %v, want refused", ends[0]["result"])
	}
}

func TestRunShellFlagsMarkerAndFoldsStatus(t *testing.T) {
	script := "dd if=/6.5.30/disc1/dist/sa bs=512 count=1\n" +
		"trap : 2 ; ( status=$? ; trap '' 2 ; echo 'o?_InstProc9IsDone\\c' ; echo 'o?_InstProc9IsDone'$status'\\c' 1>&2 )\n"
	ends := captureEnds(t, shellTestFS(), script)
	if len(ends) != 2 {
		t.Fatalf("got %d command-end events, want 2", len(ends))
	}
	if ends[0]["marker"] == true {
		t.Errorf("dd line should not be a marker")
	}
	if ends[1]["marker"] != true {
		t.Errorf("wrapper line should be flagged marker, got %v", ends[1]["marker"])
	}
}
