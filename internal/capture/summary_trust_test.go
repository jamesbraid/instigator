package capture

import (
	"strings"
	"testing"
)

func TestSummarizeFailsOnMidFileCorruption(t *testing.T) {
	good := jsonl(
		map[string]any{"event": "rsh_session_start", "session": "s1"},
		map[string]any{"event": "inst_command_end", "session": "s1", "seq": 1, "verb": "dd", "duration_ms": 10, "result": "ok"},
	)
	// a corrupt line in the middle, then a valid one after it
	in := good + "{not json at all}\n" +
		jsonl(map[string]any{"event": "rsh_session_end", "session": "s1", "result": "ok", "duration_ms": 100})

	_, err := Summarize(strings.NewReader(in))
	if err == nil {
		t.Fatal("Summarize accepted a corrupt mid-file line; want an error")
	}
	if !strings.Contains(err.Error(), "line 3") {
		t.Errorf("error should name the corrupt line number, got: %v", err)
	}
}

func TestSummarizeToleratesTruncatedFinalLine(t *testing.T) {
	// a crash can leave the last line half-written; that must not fail.
	in := jsonl(
		map[string]any{"event": "inst_command_end", "session": "s1", "seq": 1, "verb": "dd", "duration_ms": 10, "result": "ok"},
	) + `{"event":"inst_comm`
	sum, err := Summarize(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Summarize rejected a truncated final line: %v", err)
	}
	if sum.Commands != 1 {
		t.Errorf("commands = %d, want 1 (the one complete command)", sum.Commands)
	}
}

// Real inst sends each command and its completion marker as one wire line,
// so the install work lives in marker-flagged events. The summarizer must
// count those under the wrapped verb and exclude only the payload-free
// wrapper (the session-opening trap arm).
func TestSummarizeUnwrapsMarkerCommands(t *testing.T) {
	wrapper := `( status=$? ; trap '' 2 ; echo 'o?_InstProc421IsDone\c' ; echo 'o?_InstProc421IsDone'$status'\c' 1>&2 )`
	in := jsonl(
		map[string]any{"event": "rsh_session_start", "session": "s1"},
		map[string]any{"event": "inst_command_end", "session": "s1", "seq": 1, "verb": "trap",
			"line": "trap : 2 ; " + wrapper, "duration_ms": 5, "result": "ok", "marker": true},
		map[string]any{"event": "inst_command_end", "session": "s1", "seq": 2, "verb": "dd",
			"line":        "dd if=/6.5.30/dist/sa bs=512 ; " + wrapper,
			"duration_ms": 100, "stdout_bytes": 43000, "result": "ok", "marker": true},
		map[string]any{"event": "rsh_session_end", "session": "s1", "result": "ok", "duration_ms": 1000, "command_count": 2},
	)
	sum, err := Summarize(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Commands != 1 {
		t.Errorf("run commands = %d, want 1 (the wrapped dd; bare wrapper excluded)", sum.Commands)
	}
	if len(sum.Sessions) != 1 || sum.Sessions[0].Commands != 1 || sum.Sessions[0].ActiveMS != 100 {
		t.Errorf("session = %+v, want 1 command, 100ms active", sum.Sessions)
	}
	if len(sum.Sessions) == 1 && sum.Sessions[0].WallBps != 43000 {
		t.Errorf("wall throughput = %d B/s, want 43000 (dd stdout over 1s wall)", sum.Sessions[0].WallBps)
	}
	if len(sum.Verbs) != 1 || sum.Verbs[0].Verb != "dd" || sum.Verbs[0].Count != 1 {
		t.Errorf("verbs = %+v, want exactly dd x1 (no trap)", sum.Verbs)
	}
}

func TestSummarizePerPathBytesNotOvercountedForMultiFile(t *testing.T) {
	in := jsonl(
		// one grep over two files, 1000 EFS bytes total for the command
		map[string]any{"event": "inst_command_end", "session": "s1", "seq": 1, "verb": "fgrep",
			"duration_ms": 10, "result": "ok", "efs_read_bytes": 1000,
			"served": []map[string]any{
				{"tree_path": "/a", "image": "x.iso", "image_path": "a"},
				{"tree_path": "/b", "image": "x.iso", "image_path": "b"},
			}},
	)
	sum, err := Summarize(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	var total int64
	for _, p := range sum.Paths {
		total += p.Bytes
	}
	if total > 1000 {
		t.Errorf("per-path bytes total = %d, want <= 1000 (the command only read 1000); overcounted", total)
	}
}
