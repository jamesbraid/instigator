package qemunet

import (
	"bytes"
	"context"
	"net"
	"runtime"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func TestFramePumpBatchesQueuedFrames(t *testing.T) {
	for _, tc := range []struct {
		name       string
		frames     int
		wantWrites int64
	}{
		{"single", 1, 1},
		{"queued", 8, 1},
		{"bounded", 65, 3},
	} {
		for _, transport := range []string{"pipe", "tcp", "unix"} {
			if transport == "unix" && runtime.GOOS == "windows" {
				continue
			}
			t.Run(tc.name+"/"+transport, func(t *testing.T) {
				var conn, peer net.Conn
				if transport == "pipe" {
					conn, peer = net.Pipe()
				} else {
					conn, peer = socketPair(t, transport)
				}
				defer conn.Close()
				defer peer.Close()
				measured := &measuredConn{Conn: conn}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				ch := channel.New(channelQueue, 1500, testSrvMAC)
				defer ch.Close()
				want := make([][]byte, tc.frames)
				for i := range tc.frames {
					pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
						ReserveHeaderBytes: header.EthernetMinimumSize,
						Payload:            buffer.MakeWithData([]byte{byte(i)}),
					})
					hdr := pkt.LinkHeader().Push(header.EthernetMinimumSize)
					for j := range hdr {
						hdr[j] = byte(i + j)
					}
					want[i] = append(append([]byte(nil), hdr...), byte(i))
					var list stack.PacketBufferList
					list.PushBack(pkt)
					n, err := ch.WritePackets(list)
					pkt.DecRef()
					if err != nil || n != 1 {
						t.Fatalf("queue frame = %d, %v", n, err)
					}
				}
				var stream net.Conn = measured
				if transport != "pipe" {
					stream = conn
				}
				n := &Network{conn: stream, ch: ch, ctx: ctx, cancel: cancel}
				n.wg.Add(1)
				go n.writeLoop()
				defer func() {
					cancel()
					peer.Close()
					conn.Close()
					n.wg.Wait()
				}()
				peer.SetReadDeadline(time.Now().Add(5 * time.Second))
				r := frameReader{r: peer}
				for i := range want {
					got, err := r.read(maxFrame)
					if err != nil {
						t.Fatalf("frame %d: %v", i, err)
					}
					if !bytes.Equal(got, want[i]) {
						t.Fatalf("frame %d = %x, want %x", i, got, want[i])
					}
				}
				cancel()
				n.wg.Wait()
				if calls := measured.writes.Load(); transport == "pipe" && calls != tc.wantWrites {
					t.Fatalf("%d queued frames required %d writes, want %d", tc.frames, calls, tc.wantWrites)
				}
			})
		}
	}
}
