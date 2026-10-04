package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/efs/efstest"
)

func TestCLIErrorIncludesTimestamp(t *testing.T) {
	if missing := os.Getenv("INSTIGATOR_TEST_MISSING_CONFIG"); missing != "" {
		os.Args = []string{"instigator", "serve", missing}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIErrorIncludesTimestamp$")
	cmd.Env = append(os.Environ(), "INSTIGATOR_TEST_MISSING_CONFIG="+filepath.Join(t.TempDir(), "missing.yaml"))
	output, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("CLI error exit = %v; output %q", err, output)
	}
	fields := strings.Fields(string(output))
	if len(fields) < 4 {
		t.Fatalf("incomplete CLI error: %q", output)
	}
	if _, err := time.Parse(time.RFC3339, fields[0]); err != nil {
		t.Fatalf("CLI error has no timestamp: %q", output)
	}
	if fields[1] != "ERROR" || fields[2] != "instigator:" {
		t.Fatalf("CLI error has no severity or program name: %q", output)
	}
}

// The serve command's stdout is the operational server log. PROM and Inst
// commands remain available in the static guide instead of being mixed into
// that log.
func TestRunDoesNotPrintOperatorCommands(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "dist.image")
	image := efstest.New()
	sa := image.AddFile(0o444, []byte("sa"))
	image.SetRoot(map[string]uint32{"dist": image.AddDir(map[string]uint32{"sa": sa})})
	if err := os.WriteFile(imagePath, image.CDImage(64, nil), 0o644); err != nil {
		t.Fatal(err)
	}

	yaml := fmt.Sprintf(`
server_ip: 192.0.2.10
clients:
  - {name: octane, mac: "08:00:69:0e:af:12", ip: 127.0.0.1}
install_sets:
  - name: "6.5.30"
    layers:
      - {name: base, source: %q}
  - name: foundations
    layers:
      - {name: foundations, source: %q}
services:
  bootp: false
  tftp: {enabled: false}
  rsh: false
`, imagePath, imagePath)
	configPath := filepath.Join(dir, "instigator.yaml")
	if err := os.WriteFile(configPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	stop := make(chan os.Signal, 1)
	stop <- os.Interrupt
	var output bytes.Buffer
	if err := runUntilSignal(configPath, false, "", "", "", &output, stop); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "set 6.5.30: enabled") {
		t.Fatalf("server log = %q, want server startup log", got)
	}
	if strings.Contains(got, "PROM:") || strings.Contains(got, "Inst>:") {
		t.Errorf("server log includes operator commands:\n%s", got)
	}
}
