package qemunet

import (
	"bytes"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/internal/logging"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func waitForLog(t *testing.T, out *lockedBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("log never contained %q; got:\n%s", want, out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A peer that stops reading blocks the frame writer. The warning must appear
// while it is blocked, and a recovery line once the peer reads again.
func TestBlockedStreamWriteIsLoggedAndRecovery(t *testing.T) {
	old := writeStallWarn
	writeStallWarn = 50 * time.Millisecond
	t.Cleanup(func() { writeStallWarn = old })
	out := &lockedBuffer{}
	conn, peer := net.Pipe()
	n, err := New(conn, Config{
		ServerIP: netip.AddrFrom4(testSrvIP), PrefixLen: 24,
		MAC:    net.HardwareAddr([]byte(testSrvMAC)),
		Logger: logging.New(out, logging.LevelInfo),
	})
	if err != nil {
		peer.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close(); n.Close() })

	// An ARP request makes the server write a reply, which a pipe nobody
	// reads cannot take.
	request := make([]byte, 60)
	copy(request[:6], bytes.Repeat([]byte{255}, 6))
	copy(request[6:12], []byte(testGstMAC))
	copy(request[12:22], []byte{8, 6, 0, 1, 8, 0, 6, 4, 0, 1})
	copy(request[22:28], []byte(testGstMAC))
	copy(request[28:32], testGstIP[:])
	copy(request[38:42], testSrvIP[:])
	w := frameWriter{w: peer}
	if err := w.write(request); err != nil {
		t.Fatal(err)
	}
	waitForLog(t, out, "WARN  qemunet: write to the machine has been blocked")

	peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	r := frameReader{r: peer}
	if _, err := r.read(maxFrame); err != nil {
		t.Fatal(err)
	}
	waitForLog(t, out, "INFO  qemunet: write to the machine recovered after")
}

// A write that completes in time logs nothing.
func TestPromptStreamWriteIsSilent(t *testing.T) {
	out := &lockedBuffer{}
	watch := newWriteWatch(logging.New(out, logging.LevelDebug), time.Hour)
	defer watch.close()
	watch.arm()
	watch.disarm(nil)
	if got := out.String(); got != "" {
		t.Fatalf("unexpected log output: %q", got)
	}
}
