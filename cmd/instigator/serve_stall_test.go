//go:build unix

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/efs/efstest"
)

// stallingImage serves a remote EFS image whose dist/sa lies past the first
// MiB, and stops responding partway through any read there.
func stallingImage(t *testing.T) (url string, stalled <-chan struct{}) {
	t.Helper()
	b := efstest.New()
	sa := b.AddFile(0o444, nil)
	b.SetRoot(map[string]uint32{"dist": b.AddDir(map[string]uint32{"sa": sa})})
	b.AddData(make([]byte, 2<<20))
	data := bytes.Repeat([]byte("S"), 4096)
	bn, n := b.AddData(data)
	b.PutInode(sa, 0x8000|0o444, int32(len(data)), 1000+sa, [][3]uint32{{uint32(bn), n, 0}})
	img := b.CDImage(64, nil)

	ch := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var first, last int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &first, &last); err == nil && first >= 1<<20 {
			last = min(last, int64(len(img))-1)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, len(img)))
			w.Header().Set("Content-Length", fmt.Sprint(last-first+1))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(img[first : first+512])
			w.(http.Flusher).Flush()
			select {
			case ch <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		http.ServeContent(w, r, "disc.image", time.Time{}, bytes.NewReader(img))
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	return srv.URL + "/disc.image", ch
}

// A tftp transfer stuck on a stalled media server must not hold shutdown
// for the whole drain window: the stop aborts the read.
func TestServeStopAbortsStalledMediaRead(t *testing.T) {
	url, stalled := stallingImage(t)
	probe, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()
	config := filepath.Join(t.TempDir(), "instigator.yaml")
	if err := os.WriteFile(config, []byte(fmt.Sprintf(`
server_ip: 192.0.2.10
cache_dir: %q
clients:
  - {name: octane, mac: "08:00:69:0e:af:12", ip: 127.0.0.1}
install_sets:
  - name: "6.5.30"
    layers:
      - {name: base, source: %q}
services: {bootp: false, tftp: {enabled: true}, rsh: false}
ports: {tftp: %d}
`, t.TempDir(), url, port)), 0o644); err != nil {
		t.Fatal(err)
	}

	stop := make(chan os.Signal, 1)
	done := make(chan error, 1)
	var log lockedBuffer
	go func() { done <- runUntilSignal(config, false, "", "", "", nil, &log, nil, stop) }()
	waitFor(t, "serving", &log, func() bool { return strings.Contains(log.String(), "serving; stop with") })

	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rrq := binary.BigEndian.AppendUint16(nil, 1)
	rrq = append(append(append(rrq, "/6.5.30/dist/sa"...), 0), "octet\x00"...)
	if _, err := c.WriteTo(rrq, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stalled:
	case <-time.After(10 * time.Second):
		t.Fatalf("the transfer never read dist/sa:\n%s", &log)
	}

	start := time.Now()
	stop <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("serve still running 15s after the stop:\n%s", &log)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("serve took %v to stop with a stalled media read, want under 2s:\n%s", took, &log)
	}
}
