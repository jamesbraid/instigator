package nfs_test

import (
	"bytes"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/internal/sharefs"
	"github.com/jamesbraid/instigator/nfs"
)

// A UDP client that hears no reply sends the same call again. A second
// REMOVE run for real would be NOENT although the first worked, so the
// server must answer it from the first reply.
func TestUDPRetransmissionIsAnsweredFromCache(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	fsys, err := sharefs.Open("s", dir, sharefs.Options{PathTable: filepath.Join(t.TempDir(), "handles")})
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()
	_, addr := startNFSServer(t, &nfs.Server{FS: fsys})

	c, err := net.Dial("udp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	send := func(call []byte) []byte {
		t.Helper()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write(call); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 65536)
		n, err := c.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		return buf[:n]
	}
	mnt := send((&xdr{}).u32(1).u32(0).u32(2).u32(progMount).u32(1).u32(1).fixed(authNull).u32(0).u32(0).str("/s").b)
	root := mnt[28:60]
	remove := (&xdr{}).u32(2).u32(0).u32(2).u32(progNFS).u32(2).u32(10).fixed(authNull).u32(0).u32(0).fixed(root).str("f").b
	first := send(remove)
	if st := binary.BigEndian.Uint32(first[24:]); st != 0 {
		t.Fatalf("REMOVE status %d", st)
	}
	if again := send(remove); !bytes.Equal(again, first) {
		t.Fatalf("retransmitted REMOVE answered %x, want the first reply %x", again, first)
	}
	// A new call - a new xid - runs for real.
	remove[3] = 3
	if st := binary.BigEndian.Uint32(send(remove)[24:]); st != 2 {
		t.Fatalf("second REMOVE status %d, want NOENT", st)
	}
}
