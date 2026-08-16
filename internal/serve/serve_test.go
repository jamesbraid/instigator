package serve

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/efs/efstest"
	"github.com/jamesbraid/instigator/internal/config"
)

// mediaDir builds a media directory holding one synthetic disc with
// stand/fx.64 and dist/sa.
func mediaDir(t *testing.T) string {
	t.Helper()
	img := efstest.New()
	fx := img.AddFile(0o755, []byte("fake fx binary"))
	stand := img.AddDir(map[string]uint32{"fx.64": fx})
	sa := img.AddFile(0o644, bytes.Repeat([]byte("S"), 1024))
	dist := img.AddDir(map[string]uint32{"sa": sa})
	img.SetRoot(map[string]uint32{"stand": stand, "dist": dist})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Overlay 1of3.iso"), img.CDImage(64, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func testConfig(t *testing.T) *config.Config {
	yaml := fmt.Sprintf(`
server_ip: 127.0.0.1
clients:
  - {name: octane, mac: "08:00:69:0e:af:12", ip: 127.0.0.1}
media:
  - {name: "6.5.30", discs: %q}
ports: {bootp: 0, tftp: 0, rsh: 0}
`, mediaDir(t))
	c, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	// port 0 = kernel-assigned, for unprivileged tests
	c.Ports = config.Ports{}
	c.Services.NFS = true
	return c
}

func startAll(t *testing.T) (*Servers, *config.Config) {
	t.Helper()
	cfg := testConfig(t)
	// tests cannot bind reserved client ports
	s, err := Start(cfg, t.Logf, WithRSHHighPorts())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, cfg
}

func TestTFTPServesFx(t *testing.T) {
	s, _ := startAll(t)
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var req bytes.Buffer
	binary.Write(&req, binary.BigEndian, uint16(1))
	req.WriteString("/6.5.30/overlay-1of3/stand/fx.64")
	req.WriteByte(0)
	req.WriteString("octet")
	req.WriteByte(0)
	if _, err := c.WriteTo(req.Bytes(), s.TFTPAddr()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, from, err := c.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if op := binary.BigEndian.Uint16(buf[0:2]); op != 3 {
		t.Fatalf("opcode %d body %q", op, buf[4:n])
	}
	if string(buf[4:n]) != "fake fx binary" {
		t.Fatalf("data = %q", buf[4:n])
	}
	ack := make([]byte, 4)
	binary.BigEndian.PutUint16(ack, 4)
	binary.BigEndian.PutUint16(ack[2:], 1)
	c.WriteTo(ack, from)
}

func TestRSHServesDD(t *testing.T) {
	s, _ := startAll(t)
	c, err := net.Dial("tcp", s.RSHAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "0\x00guest\x00guest\x00dd if=/6.5.30/overlay-1of3/dist/sa bs=512\x00")
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, _ := io.ReadAll(c)
	if len(b) < 1 || b[0] != 0 {
		t.Fatalf("no acceptance byte: %q", b[:min(len(b), 40)])
	}
	// payload after the acceptance byte: 1024 'S' bytes, then dd's
	// records summary (stderr shares the connection without a channel)
	payload := b[1:]
	if !bytes.HasPrefix(payload, bytes.Repeat([]byte("S"), 1024)) {
		t.Fatalf("dd payload wrong, got %d bytes starting %q", len(payload), payload[:min(len(payload), 20)])
	}
	if !bytes.Contains(payload, []byte("records out")) {
		t.Fatal("records summary missing")
	}
}

func TestNFSMountAndRead(t *testing.T) {
	s, _ := startAll(t)
	pm := s.PortmapAddr().(*net.UDPAddr)

	// portmap GETPORT(mount) -> mountd, MNT(/6.5.30/overlay-1of3) -> fh
	mountPort := getport(t, pm, 100005, 1)
	fh := mountRoot(t, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: mountPort}, "/6.5.30/overlay-1of3")

	// portmap GETPORT(nfs), LOOKUP dist -> sa, READ
	nfsPort := getport(t, pm, 100003, 2)
	na := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: nfsPort}
	dist := nfsLookup(t, na, fh, "dist")
	sa := nfsLookup(t, na, dist, "sa")
	data := nfsRead(t, na, sa, 0, 1024)
	if len(data) != 1024 || data[0] != 'S' {
		t.Fatalf("read %d bytes of dist/sa", len(data))
	}
}

func getport(t *testing.T, pm *net.UDPAddr, prog, vers uint32) int {
	t.Helper()
	var a bytes.Buffer
	binary.Write(&a, binary.BigEndian, prog)
	binary.Write(&a, binary.BigEndian, vers)
	binary.Write(&a, binary.BigEndian, uint32(17))
	binary.Write(&a, binary.BigEndian, uint32(0))
	rep := rpcCall(t, pm, 100000, 2, 3, a.Bytes())
	return int(binary.BigEndian.Uint32(rep))
}

func mountRoot(t *testing.T, mnt *net.UDPAddr, path string) []byte {
	t.Helper()
	var a bytes.Buffer
	binary.Write(&a, binary.BigEndian, uint32(len(path)))
	a.WriteString(path)
	for a.Len()%4 != 0 {
		a.WriteByte(0)
	}
	rep := rpcCall(t, mnt, 100005, 1, 1, a.Bytes())
	if st := binary.BigEndian.Uint32(rep); st != 0 {
		t.Fatalf("MNT status %d", st)
	}
	return rep[4:36]
}

func nfsLookup(t *testing.T, na *net.UDPAddr, fh []byte, name string) []byte {
	t.Helper()
	var a bytes.Buffer
	a.Write(fh)
	binary.Write(&a, binary.BigEndian, uint32(len(name)))
	a.WriteString(name)
	for a.Len()%4 != 0 {
		a.WriteByte(0)
	}
	rep := rpcCall(t, na, 100003, 2, 4, a.Bytes())
	if st := binary.BigEndian.Uint32(rep); st != 0 {
		t.Fatalf("LOOKUP %q status %d", name, st)
	}
	return rep[4:36]
}

func nfsRead(t *testing.T, na *net.UDPAddr, fh []byte, off, count uint32) []byte {
	t.Helper()
	var a bytes.Buffer
	a.Write(fh)
	binary.Write(&a, binary.BigEndian, off)
	binary.Write(&a, binary.BigEndian, count)
	binary.Write(&a, binary.BigEndian, uint32(0))
	rep := rpcCall(t, na, 100003, 2, 6, a.Bytes())
	if st := binary.BigEndian.Uint32(rep); st != 0 {
		t.Fatalf("READ status %d", st)
	}
	// after status(4) + fattr(68): opaque data
	dlen := binary.BigEndian.Uint32(rep[72:])
	return rep[76 : 76+dlen]
}

// rpcCall sends one ONC-RPC/UDP call and returns the body after the
// accepted-reply header.
func rpcCall(t *testing.T, addr *net.UDPAddr, prog, vers, proc uint32, args []byte) []byte {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var b bytes.Buffer
	w := func(v uint32) { binary.Write(&b, binary.BigEndian, v) }
	w(0x33445566)
	w(0)
	w(2)
	w(prog)
	w(vers)
	w(proc)
	w(0)
	w(0)
	w(0)
	w(0)
	b.Write(args)
	if _, err := c.WriteTo(b.Bytes(), addr); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 65536)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := c.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	return buf[24:n]
}

func TestBOOTPAnswersConfiguredMAC(t *testing.T) {
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// tests cannot receive broadcasts: redirect replies at Start
	cfg := testConfig(t)
	s, err := Start(cfg, t.Logf, WithBootpReplyAddr(c.LocalAddr()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	req := make([]byte, 300)
	req[0], req[1], req[2] = 1, 1, 6
	binary.BigEndian.PutUint32(req[4:], 42)
	copy(req[28:], []byte{0x08, 0x00, 0x69, 0x0e, 0xaf, 0x12})
	if _, err := c.WriteTo(req, s.BOOTPAddr()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 400)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := c.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n < 300 || buf[0] != 2 {
		t.Fatalf("reply %d bytes op %d", n, buf[0])
	}
	if got := net.IP(buf[16:20]).String(); got != "127.0.0.1" {
		t.Fatalf("yiaddr = %s", got)
	}
}
