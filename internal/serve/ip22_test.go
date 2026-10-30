package serve

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/efs/efstest"
	"github.com/jamesbraid/instigator/internal/config"
)

// tftpGet reads one file the way a PROM does: 512-byte blocks, each acked,
// ending on the first short block.
func tftpGet(t *testing.T, s *Servers, name string) []byte {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var req bytes.Buffer
	binary.Write(&req, binary.BigEndian, uint16(1))
	req.WriteString(name)
	req.WriteByte(0)
	req.WriteString("octet")
	req.WriteByte(0)
	if _, err := c.WriteTo(req.Bytes(), sendAddr(s.TFTPAddr())); err != nil {
		t.Fatal(err)
	}
	var got []byte
	buf := make([]byte, 1500)
	for {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, from, err := c.ReadFrom(buf)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if op := binary.BigEndian.Uint16(buf[0:2]); op != 3 {
			t.Fatalf("%s: opcode %d body %q", name, op, buf[4:n])
		}
		got = append(got, buf[4:n]...)
		ack := make([]byte, 4)
		binary.BigEndian.PutUint16(ack, 4)
		copy(ack[2:], buf[2:4])
		c.WriteTo(ack, from)
		if n-4 < 512 {
			return got
		}
	}
}

// An IP22 PROM boots 32-bit ARCS programs: stand/fx.ARCS from the stand
// directory, and for an install the dist/sa archive plus the miniroot
// kernel at dist/miniroot/unix.IP22 that sash fetches. Only the file
// names differ from the 64-bit flow, so this checks they are served from
// the paths the 6.5.22 Installation Tools disc lays out.
func TestTFTPServesIP22BootFiles(t *testing.T) {
	dir := t.TempDir()
	img := efstest.New()
	fx := img.AddFile(0o755, []byte("fake fx.ARCS"))
	sa := img.AddFile(0o644, bytes.Repeat([]byte("S"), 1500))
	img.SetRoot(map[string]uint32{
		"stand": img.AddDir(map[string]uint32{"fx.ARCS": fx}),
		"dist":  img.AddDir(map[string]uint32{"sa": sa}),
	})
	// The efstest image holds 8 inodes, so the miniroot kernel comes from a
	// second layer; the served tree is the same merged dist.
	mr := efstest.New()
	unix := mr.AddFile(0o755, []byte("fake unix.IP22"))
	mr.SetRoot(map[string]uint32{"dist": mr.AddDir(map[string]uint32{
		"miniroot": mr.AddDir(map[string]uint32{"unix.IP22": unix}),
	})})
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
server_ip: 127.0.0.1
clients:
  - {name: indy, mac: "08:00:69:0e:af:12", ip: 127.0.0.1}
install_sets:
  - name: "6.5.22"
    layers:
      - {name: overlays1, source: %q, boot: true}
      - {name: miniroot, source: %q}
`, writeImage(t, dir, "overlays1.image", img), writeImage(t, dir, "miniroot.image", mr))))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Ports = config.Ports{}
	s, c := captureStart(t, cfg)

	for name, want := range map[string]string{
		"/6.5.22/stand/fx.ARCS":           "fake fx.ARCS",
		"/6.5.22/dist/miniroot/unix.IP22": "fake unix.IP22",
		"6.5.22/dist/miniroot/unix.IP22":  "fake unix.IP22",
	} {
		if got := tftpGet(t, s, name); string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := tftpGet(t, s, "/6.5.22/dist/sa"); len(got) != 1500 {
		t.Errorf("dist/sa = %d bytes, want 1500", len(got))
	}
	if !hasLine(c.instructions, "  PROM: boot -f bootp():/6.5.22/stand/fx.ARCS  (IP22: Indy, Indigo2)") ||
		!hasLine(c.instructions, "  PROM: Remote Directory: /6.5.22/dist") {
		t.Errorf("instructions lack the IP22 boot lines:\n%v", c.instructions)
	}
	if hasSubstring(c.instructions, "fx.64") {
		t.Errorf("instructions name fx.64, which this set lacks:\n%v", c.instructions)
	}
}
