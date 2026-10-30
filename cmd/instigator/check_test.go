//go:build unix

package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckAssemblesMediaWithoutBindingServices(t *testing.T) {
	held, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	port := held.LocalAddr().(*net.UDPAddr).Port
	data, err := os.ReadFile(serveConfig(t, port))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(instigator(t), "check", "-")
	cmd.Stdin = strings.NewReader(string(data))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("check with occupied BOOTP port: %v: %s", err, out)
	}

	bad := strings.Replace(string(data), "    layers:\n", "    collisions: {\"6.5.30/dist/absent\": base}\n    layers:\n", 1)
	if bad == string(data) {
		t.Fatal("could not add collision fixture")
	}
	cmd = exec.Command(instigator(t), "check", "-")
	cmd.Stdin = strings.NewReader(bad)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "6.5.30/dist/absent") {
		t.Fatalf("missing collision winner accepted or unnamed: %v: %s", err, out)
	}
}

// Add-on files are found next to the config file, whatever the working
// directory is.
func TestCheckReadsAddonFilesBesideTheConfig(t *testing.T) {
	data, err := os.ReadFile(serveConfig(t, 0))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	addon := "name: extra\nlayers:\n  - {name: extra, source: /nonexistent/extra.image}\ninstall: [extra.sw]\n"
	if err := os.WriteFile(filepath.Join(dir, "extra.yaml"), []byte(addon), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := "addons: [extra.yaml]\ninstall_scripts:\n  - name: irix\n    install: [nfs.sw.nfs]\n" + string(data)
	path := filepath.Join(dir, "instigator.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(instigator(t), "check", path)
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	// The add-on's source is absent, which is only reported if its file was read.
	if err == nil || !strings.Contains(string(out), "extra.image") {
		t.Fatalf("add-on file not read beside the config: %v: %s", err, out)
	}
}
