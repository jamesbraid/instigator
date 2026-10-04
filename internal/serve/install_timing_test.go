package serve

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/internal/capture"
	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/instcmd"
	"github.com/jamesbraid/instigator/internal/instscript"
	"github.com/jamesbraid/instigator/internal/logging"
)

func TestInstallTimingLogsStartAndReturnWithoutCapture(t *testing.T) {
	cfg := fourSetConfig(t, true)
	tree, _, err := buildTree(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	var logs bytes.Buffer
	scripts := newInstallScripts(cfg, tree, logging.New(&logs, logging.LevelInfo), nil)
	fsys := cmdFS{t: tree, scripts: scripts, client: "guest", address: "192.0.2.20"}
	fetch := func(command string) string {
		t.Helper()
		var output bytes.Buffer
		if err := instcmd.RunShell(fsys, strings.NewReader(command+"\n"), &output, io.Discard, nil, nil); err != nil {
			t.Fatal(err)
		}
		return output.String()
	}
	if _, err := fsys.Stat(installStartPath("install")); err != nil {
		t.Fatal(err)
	}
	fetch("dd if=" + installStartPath("install") + " bs=1 count=1")
	fetch("cat " + installStartPath("install") + " >/dev/null")
	if logs.Len() != 0 {
		t.Fatalf("metadata, partial, or discarded fetch logged a start: %s", &logs)
	}
	body := fetch("dd if=" + installStartPath("install") + " bs=512")
	_, returned, ok := strings.Cut(strings.TrimSpace(strings.SplitN(body, "\n", 2)[1]), ":")
	if !ok {
		t.Fatalf("start file has no return marker: %q", body)
	}
	if !strings.Contains(logs.String(), "install_start:") || strings.Contains(logs.String(), "install_returned:") {
		t.Fatalf("start did not log a separate start event: %s", &logs)
	}
	fetch("dd if=" + returned + " bs=512")
	fetch("cat " + returned) // a retry must not duplicate the return log
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("start/return log lines = %d, want 2: %s", len(lines), &logs)
	}
	attempt := strings.Split(returned, "/")[3]
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[1] != "INFO" {
			t.Fatalf("event missing INFO level: %q", line)
		}
		if _, err := time.Parse(time.RFC3339, fields[0]); err != nil {
			t.Fatalf("event missing timestamp: %q", line)
		}
		for _, value := range []string{"install.cmds", "guest", attempt} {
			if !strings.Contains(line, value) {
				t.Errorf("event missing %q: %q", value, line)
			}
		}
	}
	if !strings.Contains(lines[1], "install_returned:") || !strings.Contains(lines[1], "go returned after ") || !strings.Contains(lines[1], "success not verified") {
		t.Fatalf("return event missing duration or result boundary: %q", lines[1])
	}
}

func TestGeneratedScriptsUseInstallTimingHandoff(t *testing.T) {
	cfg := fourSetConfig(t, true)
	cfg.InstallScripts = append(cfg.InstallScripts, config.InstallScript{Name: "debug"}, config.InstallScript{Name: "maintenance"}, config.InstallScript{Name: "attempts"})
	tree, _, err := buildTree(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	scripts := newInstallScripts(cfg, tree, nil, nil)
	fsys := cmdFS{t: tree, scripts: scripts, client: "guest", address: "192.0.2.20"}
	for _, name := range []string{"install", "debug", "maintenance", "attempts"} {
		f, err := tree.Open(name + ".cmds")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		want := "admin source 192.0.2.10:/__instigator/" + name + "/start.cmds\n"
		if !strings.HasSuffix(string(body), want) {
			t.Errorf("%s does not hand off to its start marker:\n%s", name, body)
		}
		if _, err := fsys.Stat(installStartPath(name)); err != nil {
			t.Errorf("%s marker metadata: %v", name, err)
		}
	}
}

func TestInstallTimingFetchesAreBoundToClientAndAttempt(t *testing.T) {
	cfg := fourSetConfig(t, true)
	tree, _, err := buildTree(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	dir := t.TempDir()
	rec, err := capture.New(dir, capture.WithSummaryWriter(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	scripts := newInstallScripts(cfg, tree, nil, rec)
	fsy := cmdFS{t: tree, scripts: scripts, client: "guest", address: "192.0.2.20"}
	other := cmdFS{t: tree, scripts: scripts, client: "other", address: "192.0.2.21"}
	start := "/__instigator/install/start.cmds"
	// Metadata lookup must not start an attempt or issue a token.
	if _, err := fsy.Stat(start); err != nil {
		t.Fatal(err)
	}
	if got := readEvents(t, dir); len(got) != 0 {
		t.Fatalf("stat emitted events: %v", got)
	}
	read := func(fsys instcmd.FileSystem, line string) string {
		t.Helper()
		var output bytes.Buffer
		session := rec.BeginSession("guest", "192.0.2.20", "root", "root")
		if err := instcmd.RunShell(fsys, strings.NewReader(line+"\n"), &output, io.Discard, nil, session); err != nil {
			t.Fatal(err)
		}
		session.End(nil)
		return output.String()
	}
	read(fsy, "dd if="+start+" bs=1 count=1")
	read(fsy, "cat "+start+" >/dev/null")
	for _, e := range readEvents(t, dir) {
		if e["event"] == "install_start" {
			t.Fatalf("partial or discarded fetch started an install: %v", e)
		}
	}
	first := read(fsy, "dd if="+start+" bs=512")
	second := read(fsy, "cat "+start)
	if first == second {
		t.Fatalf("repeated script fetch reused its return token: %q", first)
	}
	returnPath := func(body string) string {
		t.Helper()
		line := strings.TrimSuffix(strings.SplitN(body, "\n", 2)[1], "\n")
		_, path, ok := strings.Cut(line, ":")
		if !ok || !strings.HasPrefix(body, "go\nadmin source ") {
			t.Fatalf("invalid go marker body: %q", body)
		}
		return path
	}
	p1, p2 := returnPath(first), returnPath(second)
	if _, err := other.Open(p1); err == nil {
		t.Fatal("another client could read the return marker")
	}
	if info, err := fsy.Stat(p1); err != nil || info.Size != 1 {
		t.Fatalf("return marker stat = %+v, %v", info, err)
	}
	read(fsy, "cat "+p1)
	read(fsy, "cat "+p1) // retries must not emit duplicate return events
	read(fsy, "cat "+p2)
	var starts, ends []map[string]any
	for _, e := range readEvents(t, dir) {
		switch e["event"] {
		case "install_start":
			starts = append(starts, e)
		case "install_returned":
			ends = append(ends, e)
		}
	}
	if len(starts) != 2 || len(ends) != 2 {
		t.Fatalf("starts=%v returns=%v, want two independent attempts", starts, ends)
	}
	for i := range starts {
		if starts[i]["attempt"] != ends[i]["attempt"] || ends[i]["result"] != "returned" {
			t.Errorf("attempt changed or claimed success: %v -> %v", starts[i], ends[i])
		}
	}
}

func TestCommandsTimingHandoffKeepsSelections(t *testing.T) {
	got := instscript.Commands(instscript.Params{
		ServerIP: "192.0.2.10", Sets: []string{"/dist"}, StartPath: "/__instigator/debug/start.cmds",
		Selection: instscript.Selection{Stream: "maintenance", Install: []string{"dev.sw.dbx"}, Remove: []string{"old.sw.base"}},
	})
	if !strings.Contains(got, "install maint\nkeep *\n") || !strings.Contains(got, "install dev.sw.dbx\nremove old.sw.base\nadmin source ") || strings.Contains(got, "\ngo\n") {
		t.Fatalf("timing handoff changed selections or ran go directly:\n%s", got)
	}
}

func TestRSHRecordsAllGeneratedInstallScripts(t *testing.T) {
	cfg := fourSetConfig(t, true)
	cfg.InstallScripts = []config.InstallScript{{Name: "debug"}, {Name: "maintenance", Stream: "maintenance"}}
	dir := filepath.Join(t.TempDir(), "capture")
	rec, err := capture.New(dir, capture.WithSummaryWriter(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Start(cfg, nil, withRSHHighPorts(), withRecorder(rec))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	fetch := func(path string) string {
		t.Helper()
		conn, err := net.Dial("tcp", s.RSHAddr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprint(conn, "0\x00root\x00root\x00exec /bin/sh\x00")
		fmt.Fprintf(conn, "cat %s\n", path)
		conn.(*net.TCPConn).CloseWrite()
		body, err := io.ReadAll(conn)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) == 0 || body[0] != 0 {
			t.Fatalf("rsh refused script fetch: %q", body)
		}
		return string(body[1:])
	}
	for _, name := range []string{"install", "debug", "maintenance"} {
		body := fetch("/" + name + ".cmds")
		last := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
		_, start, ok := strings.Cut(last[len(last)-1], ":")
		if !ok {
			t.Fatalf("script has no start handoff:\n%s", body)
		}
		goFile := fetch(start)
		lines := strings.Split(strings.TrimSuffix(goFile, "\n"), "\n")
		if len(lines) != 2 || lines[0] != "go" {
			t.Fatalf("invalid go file: %q", goFile)
		}
		_, returned, ok := strings.Cut(lines[1], ":")
		if !ok || fetch(returned) != "\n" {
			t.Fatalf("missing return marker in %q", goFile)
		}
	}
	// A disconnected attempt must stay incomplete, even after finalization.
	fetch(installStartPath("install"))
	if err := s.CloseWithReason("disconnected"); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	summary, err := capture.Summarize(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Installs) != 4 {
		t.Fatalf("install attempts = %+v, want four", summary.Installs)
	}
	for i, name := range []string{"install", "debug", "maintenance"} {
		attempt := summary.Installs[i]
		if attempt.Script != name+".cmds" || attempt.Result != "returned" || attempt.DurationMS == nil {
			t.Errorf("wrong install summary: %+v", attempt)
		}
	}
	if attempt := summary.Installs[3]; attempt.Result != "incomplete" || attempt.DurationMS != nil {
		t.Errorf("disconnect fabricated completion: %+v", attempt)
	}
}

type failedInstallOutput struct{}

func (failedInstallOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestFailedInstallMarkerWriteDoesNotStartAttempt(t *testing.T) {
	cfg := fourSetConfig(t, true)
	tree, _, err := buildTree(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	dir := t.TempDir()
	rec, err := capture.New(dir, capture.WithSummaryWriter(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	fsy := cmdFS{t: tree, scripts: newInstallScripts(cfg, tree, nil, rec), client: "guest", address: "192.0.2.20"}
	for _, command := range []string{"cat " + installStartPath("install"), "dd if=" + installStartPath("install") + " bs=512"} {
		instcmd.RunShell(fsy, strings.NewReader(command+"\n"), failedInstallOutput{}, io.Discard, nil, nil)
	}
	if events := readEvents(t, dir); len(events) != 0 {
		t.Fatalf("failed writes emitted install events: %v", events)
	}
}
