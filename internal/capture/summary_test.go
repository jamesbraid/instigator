package capture

import (
	"encoding/json"
	"strings"
	"testing"
)

// jsonl renders a slice of event maps as events.jsonl text.
func jsonl(events ...map[string]any) string {
	var b strings.Builder
	for _, e := range events {
		j, _ := json.Marshal(e)
		b.Write(j)
		b.WriteByte('\n')
	}
	return b.String()
}

func TestSummarizeSessionTimingAndTotals(t *testing.T) {
	in := jsonl(
		map[string]any{"event": "server_start"},
		map[string]any{"event": "rsh_session_start", "session": "s1", "client": "octane"},
		map[string]any{"event": "inst_command_end", "session": "s1", "seq": 1, "verb": "dd",
			"duration_ms": 200, "exit_status": 0, "result": "ok",
			"efs_read_calls": 2, "efs_read_bytes": 1000, "stdout_calls": 2, "stdout_bytes": 1000,
			"served": []map[string]any{{"tree_path": "/dist/sa", "image": "sa.iso", "image_path": "dist/sa"}}},
		map[string]any{"event": "inst_command_end", "session": "s1", "seq": 2, "verb": "cat",
			"duration_ms": 300, "exit_status": 0, "result": "ok",
			"efs_read_calls": 1, "efs_read_bytes": 1000, "stdout_calls": 1, "stdout_bytes": 1000,
			"served": []map[string]any{{"tree_path": "/dist/sa", "image": "sa.iso", "image_path": "dist/sa"}}},
		map[string]any{"event": "inst_command_end", "session": "s1", "seq": 3, "verb": "rm",
			"duration_ms": 0, "exit_status": 1, "result": "refused"},
		map[string]any{"event": "rsh_session_end", "session": "s1", "result": "ok",
			"duration_ms": 1000, "command_count": 3, "bytes_out": 2000},
		map[string]any{"event": "tftp_transfer_end", "result": "ok", "size": 4096, "bytes_sent": 4096, "retransmits": 3, "blocks": 8},
		map[string]any{"event": "bootp_reply", "result": "answered"},
		map[string]any{"event": "listener_exit", "listener": "tftp", "unexpected": true, "result": "error"},
		map[string]any{"event": "server_stop", "result": "clean"},
	)

	sum, err := Summarize(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	if len(sum.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sum.Sessions))
	}
	s := sum.Sessions[0]
	if s.WallMS != 1000 || s.ActiveMS != 500 || s.IdleMS != 500 {
		t.Errorf("session timing wall=%d active=%d idle=%d, want 1000/500/500", s.WallMS, s.ActiveMS, s.IdleMS)
	}
	if s.Commands != 3 || s.BytesOut != 2000 {
		t.Errorf("session commands=%d bytes=%d, want 3/2000", s.Commands, s.BytesOut)
	}

	if sum.BytesServed != 6096 {
		t.Errorf("BytesServed = %d, want 6096 (4096 tftp + 2000 rsh)", sum.BytesServed)
	}
	if sum.RefusedCommands != 1 {
		t.Errorf("RefusedCommands = %d, want 1", sum.RefusedCommands)
	}
	if sum.Transfers != 1 {
		t.Errorf("Transfers = %d, want 1", sum.Transfers)
	}
	if sum.TFTPRetransmits != 3 {
		t.Errorf("TFTPRetransmits = %d, want 3", sum.TFTPRetransmits)
	}
	if sum.ListenerExits != 1 {
		t.Errorf("ListenerExits = %d, want 1", sum.ListenerExits)
	}
	if sum.BootpAnswered != 1 {
		t.Errorf("BootpAnswered = %d, want 1", sum.BootpAnswered)
	}

	// per-verb latency: dd's worst duration is 200
	var dd *VerbLatency
	for i := range sum.Verbs {
		if sum.Verbs[i].Verb == "dd" {
			dd = &sum.Verbs[i]
		}
	}
	if dd == nil || dd.Count != 1 || dd.WorstMS != 200 {
		t.Errorf("dd verb latency = %+v, want count 1 worst 200", dd)
	}

	// path reuse: sa.iso:/dist/sa opened by dd and cat
	var reuse *PathReuse
	for i := range sum.Paths {
		if sum.Paths[i].Path == "sa.iso:/dist/sa" {
			reuse = &sum.Paths[i]
		}
	}
	if reuse == nil || reuse.Opens != 2 {
		t.Errorf("path reuse for sa.iso:/dist/sa = %+v, want 2 opens", reuse)
	}

	// human render mentions the headline numbers
	var b strings.Builder
	sum.WriteText(&b)
	out := b.String()
	if !strings.Contains(out, "refused") {
		t.Errorf("human summary missing refused count:\n%s", out)
	}
}

// Two sessions idle in the same wall-clock window must be reported per
// session, never summed into one run-wide idle figure.
func TestSummarizeIdleIsPerSessionNotSummed(t *testing.T) {
	in := jsonl(
		map[string]any{"event": "rsh_session_start", "session": "s1", "client": "a"},
		map[string]any{"event": "rsh_session_start", "session": "s2", "client": "b"},
		map[string]any{"event": "inst_command_end", "session": "s1", "seq": 1, "verb": "dd", "duration_ms": 100, "result": "ok"},
		map[string]any{"event": "inst_command_end", "session": "s2", "seq": 1, "verb": "dd", "duration_ms": 100, "result": "ok"},
		map[string]any{"event": "rsh_session_end", "session": "s1", "result": "ok", "duration_ms": 1000, "command_count": 1},
		map[string]any{"event": "rsh_session_end", "session": "s2", "result": "ok", "duration_ms": 1000, "command_count": 1},
	)
	sum, err := Summarize(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(sum.Sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(sum.Sessions))
	}
	for _, s := range sum.Sessions {
		if s.IdleMS != 900 {
			t.Errorf("session %s idle = %d, want 900 (per session)", s.ID, s.IdleMS)
		}
	}
}
