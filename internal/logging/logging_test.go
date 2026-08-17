package logging_test

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/internal/logging"
)

// lineRE matches one log line: an RFC3339/ISO8601 timestamp, a level
// word, then the message. The zone is either a numeric offset
// (2026-08-16T16:07:38-07:00) or Z for UTC (2026-08-16T23:07:38Z) - both
// are valid RFC3339, and which one Format produces depends on the host's
// zone, so a server running in UTC prints Z.
var lineRE = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:Z|[+-]\d{2}:\d{2})) +(DEBUG|INFO|WARN|ERROR) +(.*)$`)

func TestLevelsFilterBelowThreshold(t *testing.T) {
	var buf bytes.Buffer
	l := logging.New(&buf, logging.LevelInfo)

	l.Debugf("packet detail %d", 1)
	l.Infof("normal serving")
	l.Warnf("refused: %s", "bad source port")
	l.Errorf("command not supported: %s", "fgrep")

	out := buf.String()
	if strings.Contains(out, "packet detail") {
		t.Fatalf("Debugf line present at LevelInfo threshold: %q", out)
	}
	for _, want := range []string{"normal serving", "refused: bad source port", "command not supported: fgrep"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output: %q", want, out)
		}
	}
}

func TestDebugEnabledAtDebugThreshold(t *testing.T) {
	var buf bytes.Buffer
	l := logging.New(&buf, logging.LevelDebug)
	l.Debugf("recv %d bytes from %s", 64, "10.0.0.1")
	if !strings.Contains(buf.String(), "recv 64 bytes from 10.0.0.1") {
		t.Fatalf("Debugf suppressed at LevelDebug threshold: %q", buf.String())
	}
}

func TestLineFormatISO8601AndLevel(t *testing.T) {
	var buf bytes.Buffer
	l := logging.New(&buf, logging.LevelInfo)
	l.Errorf("fgrep: not supported")

	line := strings.TrimRight(buf.String(), "\n")
	m := lineRE.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("line %q does not match <ISO8601> <LEVEL> <message>", line)
	}
	if m[2] != "ERROR" {
		t.Fatalf("level = %q, want ERROR", m[2])
	}
	if m[3] != "fgrep: not supported" {
		t.Fatalf("message = %q, want %q", m[3], "fgrep: not supported")
	}
	if _, err := time.Parse(time.RFC3339, m[1]); err != nil {
		t.Fatalf("timestamp %q does not parse as RFC3339: %v", m[1], err)
	}
}

func TestEnabledReflectsThreshold(t *testing.T) {
	l := logging.New(&bytes.Buffer{}, logging.LevelInfo)
	if l.Enabled(logging.LevelDebug) {
		t.Fatal("LevelDebug reported enabled at an Info threshold")
	}
	if !l.Enabled(logging.LevelWarn) {
		t.Fatal("LevelWarn reported disabled at an Info threshold")
	}
}

// TestNilLoggerIsSafe mirrors the old Logf-nil tolerance: a Server whose
// Logger field is never set (most tests, and any caller that doesn't
// care about logging) must not panic when a call site logs anyway.
func TestNilLoggerIsSafe(t *testing.T) {
	var l *logging.Logger
	l.Debugf("x")
	l.Infof("x")
	l.Warnf("x")
	l.Errorf("x")
	if l.Enabled(logging.LevelError) {
		t.Fatal("nil Logger reported a level enabled")
	}
}

// TestErrorAlwaysVisibleAtDefaultLevel is the headline requirement: a
// refused/unsupported command must show up in the server's own log by
// default, not just on the client's side - so Errorf at the default
// (Info) level must never be filtered out.
func TestErrorAlwaysVisibleAtDefaultLevel(t *testing.T) {
	var buf bytes.Buffer
	l := logging.New(&buf, logging.LevelInfo)
	l.Errorf("fgrep: not supported")
	if !strings.Contains(buf.String(), "ERROR") || !strings.Contains(buf.String(), "fgrep: not supported") {
		t.Fatalf("refusal did not reach the server log at the default level: %q", buf.String())
	}
}

func TestConcurrentWritesDoNotInterleave(t *testing.T) {
	var buf bytes.Buffer
	l := logging.New(&buf, logging.LevelInfo)
	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func(i int) {
			l.Infof("worker %d did some work with a reasonably long message", i)
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 20; i++ {
		<-done
	}
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if lineRE.FindStringSubmatch(line) == nil {
			t.Fatalf("interleaved or malformed line: %q", line)
		}
	}
}
