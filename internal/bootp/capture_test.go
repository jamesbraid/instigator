package bootp

import (
	"bufio"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/internal/capture"
)

func waitForEvent(t *testing.T, dir, name string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f, err := os.Open(filepath.Join(dir, "events.jsonl")); err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				var m map[string]any
				if json.Unmarshal(sc.Bytes(), &m) == nil && m["event"] == name {
					f.Close()
					return m
				}
			}
			f.Close()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("event %q never appeared in %s", name, dir)
	return nil
}

// serveOnce starts a Server on a loopback socket, sends one request to it
// from a fresh client socket, and returns after the datagram is sent. The
// server records from its own goroutine; the caller polls for the event.
func serveOnce(t *testing.T, s *Server, req []byte) {
	t.Helper()
	// Replies are sent to a throwaway socket so nothing broadcasts.
	replyConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { replyConn.Close() })
	s.ReplyAddr = replyConn.LocalAddr()

	srv, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	go s.Serve(srv)

	cl, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	if _, err := cl.WriteTo(req, srv.LocalAddr()); err != nil {
		t.Fatal(err)
	}
}

func TestServeRecordsAnsweredReply(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	rec, err := capture.New(dir)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	defer rec.Close()

	s := &Server{
		ServerIP: serverIP,
		Netmask:  netip.MustParsePrefix("192.0.2.0/24"),
		Clients:  []Client{{Name: "octane", MAC: octaneMAC, IP: octaneIP}},
		Recorder: rec,
	}
	serveOnce(t, s, request(octaneMAC, 0x1234, "/stand/fx.64"))

	e := waitForEvent(t, dir, "bootp_reply")
	if e["result"] != "answered" {
		t.Errorf("result = %v, want answered", e["result"])
	}
	if e["client"] != "octane" {
		t.Errorf("client = %v, want octane", e["client"])
	}
	if e["requested_file"] != "/stand/fx.64" {
		t.Errorf("requested_file = %v", e["requested_file"])
	}
	if e["offered_ip"] != octaneIP.String() {
		t.Errorf("offered_ip = %v, want %s", e["offered_ip"], octaneIP)
	}
}

func TestServeRecordsIgnoredReply(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cap")
	rec, err := capture.New(dir)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	defer rec.Close()

	s := &Server{
		ServerIP: serverIP,
		Clients:  []Client{{Name: "octane", MAC: octaneMAC, IP: octaneIP}},
		Recorder: rec,
	}
	stranger := net.HardwareAddr{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01}
	serveOnce(t, s, request(stranger, 1, "/whatever"))

	e := waitForEvent(t, dir, "bootp_reply")
	if e["result"] != "ignored" {
		t.Errorf("result = %v, want ignored", e["result"])
	}
	if e["client"] != nil && e["client"] != "" {
		t.Errorf("ignored reply should have no client alias, got %v", e["client"])
	}
}
