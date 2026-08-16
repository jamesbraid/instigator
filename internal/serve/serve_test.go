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
