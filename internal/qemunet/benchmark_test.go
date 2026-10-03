package qemunet

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/internal/instcmd"
	"github.com/jamesbraid/instigator/internal/logging"
)

type measuredConn struct {
	net.Conn
	writes atomic.Int64
}

func (c *measuredConn) Write(p []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(p)
}

// Unlike net.Pipe, these transports include the socket I/O used by QEMU.
// Both ends use netstack, so results include a synthetic guest's stack cost.
func BenchmarkTCPTransfer(b *testing.B) {
	for _, transport := range []string{"tcp", "unix"} {
		if transport == "unix" && runtime.GOOS == "windows" {
			continue
		}
		for _, chunk := range []int{512, 4096, 32768, 65536} {
			b.Run(fmt.Sprintf("%s/%d", transport, chunk), func(b *testing.B) {
				benchmarkTCPTransfer(b, transport, chunk, false, false)
			})
		}
	}
}

// This runs the real shell's dd and trailing marker through the frame pumps.
func BenchmarkShellTransfer(b *testing.B) {
	for _, transport := range []string{"tcp", "unix"} {
		if transport == "unix" && runtime.GOOS == "windows" {
			continue
		}
		b.Run(transport, func(b *testing.B) {
			benchmarkTCPTransfer(b, transport, 512, true, false)
		})
	}
}

// Concrete sockets exclude the measured wrapper's per-Write counter overhead.
func BenchmarkShellTransferBare(b *testing.B) {
	for _, transport := range []string{"tcp", "unix"} {
		if transport == "unix" && runtime.GOOS == "windows" {
			continue
		}
		b.Run(transport, func(b *testing.B) {
			benchmarkTCPTransfer(b, transport, 512, true, true)
		})
	}
}

type benchmarkFS struct{ payload []byte }

func (f benchmarkFS) Open(path string) (instcmd.File, error) {
	if path != "data" {
		return nil, instcmd.ErrNotFound
	}
	return bytes.NewReader(f.payload), nil
}

func (f benchmarkFS) ReadDir(string) ([]string, error) { return nil, instcmd.ErrNotFound }

func (f benchmarkFS) Stat(path string) (instcmd.FileInfo, error) {
	if path != "data" {
		return instcmd.FileInfo{}, instcmd.ErrNotFound
	}
	return instcmd.FileInfo{Size: int64(len(f.payload))}, nil
}

func benchmarkTCPTransfer(b *testing.B, transport string, chunk int, shell, bare bool) {
	address := "127.0.0.1:0"
	if transport == "unix" {
		dir, err := os.MkdirTemp("", "qn-")
		if err != nil {
			b.Fatal(err)
		}
		defer os.RemoveAll(dir)
		address = filepath.Join(dir, "s")
	}
	ln, err := net.Listen(transport, address)
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial(transport, ln.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	defer client.Close()
	server, err := ln.Accept()
	if err != nil {
		b.Fatal(err)
	}
	defer server.Close()
	measured := &measuredConn{Conn: server}
	var stream net.Conn = measured
	if bare {
		stream = server
	}
	srv, err := New(stream, Config{ServerIP: netip.AddrFrom4(testSrvIP), PrefixLen: 24, MAC: net.HardwareAddr([]byte(testSrvMAC))})
	if err != nil {
		b.Fatal(err)
	}
	defer srv.Close()
	gst, err := New(client, Config{ServerIP: netip.AddrFrom4(testGstIP), PrefixLen: 24, MAC: net.HardwareAddr([]byte(testGstMAC))})
	if err != nil {
		b.Fatal(err)
	}
	defer gst.Close()
	tcpLn, err := srv.Listen(514)
	if err != nil {
		b.Fatal(err)
	}
	defer tcpLn.Close()
	receiver, err := gst.DialStderr(testSrvIPAddr(), 514)
	if err != nil {
		b.Fatal(err)
	}
	defer receiver.Close()
	sender, err := tcpLn.Accept()
	if err != nil {
		b.Fatal(err)
	}
	defer sender.Close()

	payload := make([]byte, 1024*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	want := payload
	const marker = "transfer-end"
	if shell {
		want = append(append([]byte(nil), payload...), marker...)
	}
	fsys := benchmarkFS{payload: payload}
	logger := logging.New(io.Discard, logging.LevelInfo)
	got := make([]byte, len(want))
	result := make(chan error, 1)
	go func() {
		for range b.N {
			receiver.SetReadDeadline(time.Now().Add(30 * time.Second))
			_, err := io.ReadFull(receiver, got)
			if err == nil && !bytes.Equal(got, want) {
				err = fmt.Errorf("received payload differs")
			}
			result <- err
			if err != nil {
				return
			}
		}
	}()
	writes := measured.writes.Load()
	segments := srv.stack.Stats().TCP.SegmentsSent.Value()
	retransmits := srv.stack.Stats().TCP.Retransmits.Value()
	sendErrors := srv.stack.Stats().TCP.SegmentSendErrors.Value()
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		sender.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if shell {
			if err := instcmd.RunShell(fsys, strings.NewReader("dd if=/data bs=512\necho 'transfer-end\\c'\n"), sender, io.Discard, logger, nil); err != nil {
				b.Fatal(err)
			}
		} else {
			for off := 0; off < len(payload); off += chunk {
				if err := writeFull(sender, payload[off:off+chunk]); err != nil {
					b.Fatal(err)
				}
			}
		}
		if err := <-result; err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if !bare {
		b.ReportMetric(float64(measured.writes.Load()-writes)/float64(b.N), "stream-writes/MiB")
	}
	b.ReportMetric(float64(srv.stack.Stats().TCP.SegmentsSent.Value()-segments)/float64(b.N), "TCP-segments/MiB")
	b.ReportMetric(float64(srv.stack.Stats().TCP.Retransmits.Value()-retransmits)/float64(b.N), "TCP-retransmits/MiB")
	b.ReportMetric(float64(srv.stack.Stats().TCP.SegmentSendErrors.Value()-sendErrors)/float64(b.N), "TCP-send-errors/MiB")
}
