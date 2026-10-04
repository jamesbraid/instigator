package capture

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestInstallEventsRecordAttemptAndReturnedDuration(t *testing.T) {
	dir := t.TempDir()
	r, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	r.InstallStart("a1", "octane", "/inst/install")
	r.InstallReturned("a1", "octane", "/inst/install", 0)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, dir)
	if len(lines) != 2 {
		t.Fatalf("events = %d, want 2", len(lines))
	}
	for i, line := range lines {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event["attempt"] != "a1" || event["client"] != "octane" || event["script"] != "/inst/install" {
			t.Errorf("install identity = %v", event)
		}
		if i == 0 {
			if _, hasDuration := event["duration_ms"]; event["event"] != "install_start" || hasDuration {
				t.Errorf("start event = %v", event)
			}
		} else if event["event"] != "install_returned" || event["result"] != "returned" || event["duration_ms"] != float64(0) {
			t.Errorf("returned event = %v", event)
		}
	}
}

func TestSummarizeInstallsPreservesStartsAndFirstMatchingReturn(t *testing.T) {
	in := jsonl(
		map[string]any{"event": "install_returned", "attempt": "orphan", "client": "octane", "script": "/inst/install", "duration_ms": 10, "result": "returned"},
		map[string]any{"event": "install_start", "ts": "2026-10-03T08:00:00Z", "attempt": "a1", "client": "octane", "script": "/inst/install"},
		map[string]any{"event": "install_start", "ts": "2026-10-03T08:00:01Z", "attempt": "a2", "client": "indy", "script": "/inst/install"},
		map[string]any{"event": "install_start", "ts": "2026-10-03T08:00:02Z", "attempt": "a3", "client": "octane", "script": "/inst/install"},
		map[string]any{"event": "install_start", "ts": "2026-10-03T08:00:03Z", "attempt": "a1", "client": "octane", "script": "/inst/install"},
		map[string]any{"event": "install_returned", "attempt": "other", "client": "octane", "script": "/inst/install", "duration_ms": 10000},
		map[string]any{"event": "install_returned", "attempt": "a3", "client": "indy", "script": "/inst/install", "duration_ms": 10000},
		map[string]any{"event": "install_returned", "attempt": "a3", "client": "octane", "script": "/inst/other", "duration_ms": 10000},
		map[string]any{"event": "install_returned", "ts": "2026-10-03T08:00:04Z", "attempt": "a2", "client": "indy", "script": "/inst/install", "duration_ms": 3000, "result": "returned"},
		map[string]any{"event": "install_returned", "ts": "2026-10-03T08:00:05Z", "attempt": "a1", "client": "octane", "script": "/inst/install", "duration_ms": 0, "result": "returned"},
		map[string]any{"event": "install_returned", "ts": "2026-10-03T08:00:06Z", "attempt": "a1", "client": "octane", "script": "/inst/install", "duration_ms": 6000, "result": "returned"},
		map[string]any{"event": "server_stop", "result": "interrupted"},
	)
	sum, err := Summarize(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"attempt": "a1", "client": "octane", "script": "/inst/install", "started": "2026-10-03T08:00:00Z", "returned": "2026-10-03T08:00:05Z", "duration_ms": float64(0), "result": "returned"},
		map[string]any{"attempt": "a2", "client": "indy", "script": "/inst/install", "started": "2026-10-03T08:00:01Z", "returned": "2026-10-03T08:00:04Z", "duration_ms": float64(3000), "result": "returned"},
		map[string]any{"attempt": "a3", "client": "octane", "script": "/inst/install", "started": "2026-10-03T08:00:02Z", "returned": "", "result": "incomplete"},
	}
	if !reflect.DeepEqual(decoded["installs"], want) {
		t.Errorf("installs = %#v, want %#v", decoded["installs"], want)
	}
	var out strings.Builder
	sum.WriteText(&out)
	for _, fragment := range []string{"/inst/install", "octane", "indy", "a1", "a2", "a3", "go returned", "0s", "3s", "incomplete", "not verified success"} {
		if !strings.Contains(out.String(), fragment) {
			t.Errorf("text summary missing %q:\n%s", fragment, out.String())
		}
	}
}

func TestSummarizeWithoutInstallsOmitsInstallField(t *testing.T) {
	sum, err := Summarize(strings.NewReader(jsonl(map[string]any{"event": "server_stop", "result": "clean"})))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"installs"`) {
		t.Errorf("empty installs changed legacy summary: %s", encoded)
	}
}
