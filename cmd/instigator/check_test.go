//go:build unix

package main

import (
	"net"
	"os"
	"os/exec"
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
