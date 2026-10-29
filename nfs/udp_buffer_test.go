package nfs_test

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"

	"github.com/jamesbraid/instigator/nfs"
)

// A 32 KiB READ reply is bigger than the send buffer macOS gives a UDP
// socket by default, 9216 bytes. ServeUDP must enlarge it, or the send
// fails and the client never gets an answer. The listener starts with a
// buffer smaller still, so the buffer check needs the raise on any host.
func TestServeUDPCarriesALargeReadReply(t *testing.T) {
	const size = 40000
	file := &memNode{id: 10, mode: 0o100644, size: size, mtime: 111,
		data: bytes.Repeat([]byte("F"), size)}
	root := &memNode{id: 1, mode: 0o040755, mtime: 100,
		entries: map[string]*memNode{"big": file}}
	fs := &memFS{root: root, byID: map[uint64]*memNode{1: root, 10: file}}

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	if err := pc.(*net.UDPConn).SetWriteBuffer(4096); err != nil {
		t.Fatal(err)
	}
	s := &nfs.Server{FS: fs}
	go s.ServeUDP(pc)
	port := uint32(pc.LocalAddr().(*net.UDPAddr).Port)
	s.SetPorts(nfs.Ports{Portmap: port, Mount: port, NFS: port}, nfs.Ports{})
	addr := pc.LocalAddr().(*net.UDPAddr)

	fh := mnt(t, s, addr, "/")
	var args bytes.Buffer
	binary.Write(&args, binary.BigEndian, uint32(len(fh))) // NFSv3 handles are counted
	args.Write(fh)
	binary.Write(&args, binary.BigEndian, uint32(len("big")))
	args.WriteString("big\x00")
	rep := rpcCall(t, addr, 100003, 3, 3, args.Bytes()) // LOOKUP
	if st := u32(rep, 0); st != 0 {
		t.Fatalf("LOOKUP status = %d", st)
	}
	fh = rep[8 : 8+u32(rep, 4)]

	args.Reset()
	binary.Write(&args, binary.BigEndian, uint32(len(fh)))
	args.Write(fh)
	binary.Write(&args, binary.BigEndian, uint64(0))     // offset
	binary.Write(&args, binary.BigEndian, uint32(32768)) // count
	rep = rpcCall(t, addr, 100003, 3, 6, args.Bytes())
	if st := u32(rep, 0); st != 0 {
		t.Fatalf("READ status = %d", st)
	}
	if len(rep) < 32768 {
		t.Fatalf("READ reply is %d bytes, want at least 32768", len(rep))
	}
	// Linux sends the datagram even from the 4096-byte buffer, so the
	// reply alone does not show the raise; macOS would drop it.
	if got := sendBuffer(pc); got >= 0 && got < 32768+512 {
		t.Fatalf("SO_SNDBUF = %d after ServeUDP, want room for a 32 KiB reply", got)
	}
}
