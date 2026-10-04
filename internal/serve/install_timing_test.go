package serve

import (
	"bytes"
	"encoding/json"
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

func sourcedPath(t *testing.T, body string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(body), "\n")
	source, ok := strings.CutPrefix(lines[len(lines)-1], "admin source ")
	_, path, remote := strings.Cut(source, ":")
	if !ok || !remote || !strings.HasPrefix(path, "/__instigator/") {
		t.Fatalf("command file has no timing handoff: %q", body)
	}
	return path
}

func TestInstallTimingTransfersAndLogs(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("capture=%t", enabled), func(t *testing.T) {
			cfg := fourSetConfig(t, true)
			tree, profile, err := buildTree(cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { tree.Close() })
			dir := t.TempDir()
			var rec *capture.Recorder
			if enabled {
				rec, err = capture.New(dir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { rec.Close() })
			}
			var logs bytes.Buffer
			scripts := newInstallScripts(cfg.ServerIP.String(), profile.generated, logging.New(&logs, logging.LevelInfo), rec)
			fsys := cmdFS{t: tree, scripts: scripts, client: "guest", address: "192.0.2.20"}
			fetch := func(command string) string {
				t.Helper()
				var output bytes.Buffer
				session := rec.BeginSession("guest", fsys.address, "root", "root")
				defer session.End(nil)
				if err := instcmd.RunShell(fsys, strings.NewReader(command+"\n"), &output, io.Discard, nil, session); err != nil {
					t.Fatal(err)
				}
				return output.String()
			}
			start := installStartPath("install")
			if _, err := fsys.Stat(start); err != nil {
				t.Fatal(err)
			}
			for _, command := range []string{"dd if=" + start + " bs=1 count=1", "cat " + start + " >/dev/null", "cat " + start + " | cat"} {
				fetch(command)
			}
			for _, command := range []string{"cat " + start, "dd if=" + start + " bs=512"} {
				instcmd.RunShell(fsys, strings.NewReader(command+"\n"), failedInstallOutput{}, io.Discard, nil, nil)
			}
			if logs.Len() != 0 {
				t.Fatalf("metadata, partial, discarded, piped, or failed transfer logged an attempt: %s", &logs)
			}
			returns := []string{sourcedPath(t, fetch("dd if="+start+" bs=512")), sourcedPath(t, fetch("cat "+start))}
			if returns[0] == returns[1] || strings.Count(logs.String(), "install_start:") != 2 || strings.Contains(logs.String(), "install_returned:") {
				t.Fatalf("repeated starts are not independent: %v\n%s", returns, &logs)
			}
			other := fsys
			other.address = "192.0.2.21"
			if _, err := other.Open(returns[0]); err == nil {
				t.Fatal("another client could read the return marker")
			}
			if info, err := fsys.Stat(returns[0]); err != nil || info.Size != 1 {
				t.Fatalf("return metadata = %+v, %v", info, err)
			}
			fetch("dd if=" + returns[0] + " bs=1 skip=1")
			if strings.Contains(logs.String(), "install_returned:") {
				t.Fatalf("metadata or partial read logged a return: %s", &logs)
			}
			fetch("dd if=" + returns[0] + " bs=512")
			fetch("cat " + returns[0]) // retries must not duplicate the return
			fetch("cat " + returns[1])
			var events []map[string]any
			if enabled {
				for _, event := range readEvents(t, dir) {
					if event["event"] == "install_start" || event["event"] == "install_returned" {
						events = append(events, event)
					}
				}
				if len(events) != 4 {
					t.Fatalf("capture events = %v, want two pairs", events)
				}
			}
			lines := splitNonEmpty(logs.String())
			if len(lines) != 4 {
				t.Fatalf("start/return logs = %v, want two pairs", lines)
			}
			for i, line := range lines {
				attempt := strings.Split(returns[i%2], "/")[3]
				fields := strings.Fields(line)
				if len(fields) < 3 || fields[1] != "INFO" {
					t.Fatalf("event missing INFO level: %q", line)
				}
				if _, err := time.Parse(time.RFC3339, fields[0]); err != nil {
					t.Fatalf("event missing timestamp: %q", line)
				}
				if !strings.Contains(line, "install.cmds (guest)") || !strings.Contains(line, attempt) {
					t.Errorf("event missing identity: %q", line)
				}
				if i >= 2 && (!strings.Contains(line, "install_returned:") || !strings.Contains(line, "go returned after ") || !strings.Contains(line, "success not verified")) {
					t.Errorf("return missing duration or result boundary: %q", line)
				}
				if enabled {
					kind := "install_start"
					if i >= 2 {
						kind = "install_returned"
					}
					if events[i]["event"] != kind || events[i]["attempt"] != attempt || (i >= 2 && events[i]["result"] != "returned") {
						t.Errorf("capture mismatched %s: %v", kind, events[i])
					}
				}
			}
		})
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
	cfg.InstallScripts = []config.InstallScript{{Name: "debug"}, {Name: "maintenance", Stream: "maintenance"}, {Name: "attempts"}}
	dir := t.TempDir()
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
		fmt.Fprintf(conn, "ls -ln %s >/dev/null && cat %s\n", path, path)
		conn.(*net.TCPConn).CloseWrite()
		body, err := io.ReadAll(conn)
		if err != nil || len(body) == 0 || body[0] != 0 {
			t.Fatalf("rsh fetch %s: %q, %v", path, body, err)
		}
		return string(body[1:])
	}
	names := []string{"install", "debug", "maintenance", "attempts"}
	for _, name := range names {
		start := sourcedPath(t, fetch("/"+name+".cmds"))
		if start != installStartPath(name) {
			t.Fatalf("%s start path = %s", name, start)
		}
		goFile := fetch(start)
		if !strings.HasPrefix(goFile, "go\n") || strings.Count(goFile, "\n") != 2 || fetch(sourcedPath(t, goFile)) != "\n" {
			t.Fatalf("invalid go/return handoff: %q", goFile)
		}
	}
	fetch(installStartPath("install")) // disconnect must leave this incomplete
	if err := s.CloseWithReason("disconnected"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary capture.Summary
	if err := json.Unmarshal(body, &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary.Installs) != len(names)+1 {
		t.Fatalf("install attempts = %+v", summary.Installs)
	}
	for i, name := range names {
		attempt := summary.Installs[i]
		if attempt.Script != name+".cmds" || attempt.Result != "returned" || attempt.DurationMS == nil {
			t.Errorf("wrong install summary: %+v", attempt)
		}
	}
	if attempt := summary.Installs[len(names)]; attempt.Result != "incomplete" || attempt.DurationMS != nil {
		t.Errorf("disconnect fabricated completion: %+v", attempt)
	}
}

type failedInstallOutput struct{}

func (failedInstallOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
