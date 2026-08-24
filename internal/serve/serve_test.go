package serve

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/efs/efstest"
	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/instcmd"
	"github.com/jamesbraid/instigator/internal/logging"
	"github.com/jamesbraid/instigator/internal/tftp"
)

// testLogWriter routes leveled log lines to t.Log, so a test failure's
// output includes the server log around it, same as the old t.Logf
// callback did.
type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// testLogger builds a Logger for tests: DEBUG level, since a test
// exercising Verbose-only behavior needs it, and t.Log as the sink so
// server log output only surfaces when a test actually fails.
func testLogger(t *testing.T) *logging.Logger {
	t.Helper()
	return logging.New(testLogWriter{t}, logging.LevelDebug)
}

// syncBuffer is a mutex-protected buffer: a test reads back what a
// server captured after Close, but background service goroutines can
// still be logging or writing operator instructions up to that point,
// so both the writes and the read need to be serialized against each
// other, not just against themselves.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// writeImage lays a built image out as CD media on disk.
func writeImage(t *testing.T, dir, name string, img *efstest.Builder) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, img.CDImage(64, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// baseImage writes the image a set's boot layer draws from: the miniroot
// partitioner at stand/fx.64 (served at the set's stand because the layer
// is boot) and a distribution at dist/sa, which is the shape the
// Installation Tools disc has in miniature.
func baseImage(t *testing.T, dir, name string) string {
	t.Helper()
	img := efstest.New()
	fx := img.AddFile(0o755, []byte("fake fx binary"))
	stand := img.AddDir(map[string]uint32{"fx.64": fx})
	sa := img.AddFile(0o644, bytes.Repeat([]byte("S"), 1024))
	dist := img.AddDir(map[string]uint32{"sa": sa})
	img.SetRoot(map[string]uint32{"stand": stand, "dist": dist})
	return writeImage(t, dir, name, img)
}

// distImage writes a later layer's image: one product under dist and
// nothing else, the shape every set after the base contributes.
func distImage(t *testing.T, dir, name, product string) string {
	t.Helper()
	img := efstest.New()
	f := img.AddFile(0o644, []byte(product))
	img.SetRoot(map[string]uint32{"dist": img.AddDir(map[string]uint32{product: f})})
	return writeImage(t, dir, name, img)
}

// testConfig serves two install sets: a bootable base set whose one layer
// also supplies stand/fx.64, and an applications set that only merges its
// dist.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	yaml := fmt.Sprintf(`
server_ip: 127.0.0.1
clients:
  - {name: octane, mac: "08:00:69:0e:af:12", ip: 127.0.0.1}
install_sets:
  - name: "6.5.30"
    layers:
      - {name: base, image: %q, boot: true}
  - name: applications
    layers:
      - {name: apps, image: %q}
`, baseImage(t, dir, "base.image"), distImage(t, dir, "apps.image", "apps.sw"))
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
	s, err := Start(cfg, testLogger(t), WithRSHHighPorts())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, cfg
}

// sendAddr rewrites a wildcard listener address (0.0.0.0) to loopback so a
// test can send UDP to it. Sending to 0.0.0.0 is quietly routed to localhost
// on Linux, but macOS returns "no route to host" and Windows "address not
// valid". The listeners really do bind the wildcard, so this fixup lives in
// the test rather than in the address the server reports.
func sendAddr(a net.Addr) *net.UDPAddr {
	u := a.(*net.UDPAddr)
	ip := u.IP
	if ip == nil || ip.IsUnspecified() {
		ip = net.IPv4(127, 0, 0, 1)
	}
	return &net.UDPAddr{IP: ip, Port: u.Port}
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
	req.WriteString("/6.5.30/stand/fx.64")
	req.WriteByte(0)
	req.WriteString("octet")
	req.WriteByte(0)
	if _, err := c.WriteTo(req.Bytes(), sendAddr(s.TFTPAddr())); err != nil {
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

// inst only ever talks to the server through "exec /bin/sh"; a bare rsh
// command is refused with a message the client sees.
func TestRSHRefusesNonShellCommand(t *testing.T) {
	s, _ := startAll(t)
	c, err := net.Dial("tcp", s.RSHAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "0\x00guest\x00guest\x00dd if=/6.5.30/dist/sa bs=512\x00")
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, _ := io.ReadAll(c)
	if len(b) < 1 || b[0] != 0 {
		t.Fatalf("no acceptance byte: %q", b[:min(len(b), 40)])
	}
	if !bytes.Contains(b[1:], []byte("only a shell session")) {
		t.Fatalf("refusal not reported to client: %q", b[1:])
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
	s, err := Start(cfg, testLogger(t), WithBootpReplyAddr(c.LocalAddr()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	req := make([]byte, 300)
	req[0], req[1], req[2] = 1, 1, 6
	binary.BigEndian.PutUint32(req[4:], 42)
	copy(req[28:], []byte{0x08, 0x00, 0x69, 0x0e, 0xaf, 0x12})
	if _, err := c.WriteTo(req, sendAddr(s.BOOTPAddr())); err != nil {
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

// A tree directory is a legitimate path, but neither protocol can send
// one as a byte stream, so the adapters have to refuse it as something
// other than "not found" - a client that asked for a directory should
// not be told the path is missing.
func TestAdaptersRefuseDirectories(t *testing.T) {
	s, _ := startAll(t)
	if _, err := (treeFS{s.tree}).Open("/6.5.30/dist"); err == nil {
		t.Error("tftp adapter opened a directory")
	} else if errors.Is(err, tftp.ErrNotFound) {
		t.Errorf("tftp adapter reported a directory as missing: %v", err)
	}
	if _, err := (cmdFS{s.tree}).Open("/6.5.30/dist"); err == nil {
		t.Error("instcmd adapter opened a directory")
	} else if errors.Is(err, instcmd.ErrNotFound) {
		t.Errorf("instcmd adapter reported a directory as missing: %v", err)
	}
}

func TestAdaptersReportMissingPaths(t *testing.T) {
	s, _ := startAll(t)
	if _, err := (treeFS{s.tree}).Open("/6.5.30/stand/fx.32"); !errors.Is(err, tftp.ErrNotFound) {
		t.Errorf("tftp adapter: err = %v, want tftp.ErrNotFound", err)
	}
	if _, err := (cmdFS{s.tree}).Open("/6.5.30/stand/fx.32"); !errors.Is(err, instcmd.ErrNotFound) {
		t.Errorf("instcmd adapter: err = %v, want instcmd.ErrNotFound", err)
	}
	if _, err := (cmdFS{s.tree}).ReadDir("/nowhere"); !errors.Is(err, instcmd.ErrNotFound) {
		t.Errorf("instcmd adapter ReadDir: err = %v, want instcmd.ErrNotFound", err)
	}
	if _, err := (cmdFS{s.tree}).Stat("/nowhere"); !errors.Is(err, instcmd.ErrNotFound) {
		t.Errorf("instcmd adapter Stat: err = %v, want instcmd.ErrNotFound", err)
	}
}

// The rsh shell lists a directory by name and stats each entry, and it
// addresses the tree root as "" once the leading slash is stripped.
func TestCmdFSListsAndStats(t *testing.T) {
	s, _ := startAll(t)
	f := cmdFS{s.tree}
	names, err := f.ReadDir("")
	if err != nil {
		t.Fatalf("ReadDir(root): %v", err)
	}
	for _, want := range []string{"6.5.30", "applications"} {
		if !hasLine(names, want) {
			t.Errorf("root listing missing %q, got %v", want, names)
		}
	}
	info, err := f.Stat("/6.5.30/dist/sa")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.IsDir || info.Size != 1024 || info.Ino == 0 || info.Nlink == 0 {
		t.Errorf("stat of dist/sa = %+v", info)
	}
	dir, err := f.Stat("/6.5.30/dist")
	if err != nil {
		t.Fatalf("Stat(dir): %v", err)
	}
	if !dir.IsDir {
		t.Errorf("stat of dist = %+v, want a directory", dir)
	}
}

// The served-file manifest line names the layer a file came from, so a
// media-backed path resolves and a generated one - which has no backing
// layer to name - reports an error instead of a made-up source.
func TestCmdFSResolvesOrigins(t *testing.T) {
	s, _ := startAll(t)
	f := cmdFS{s.tree}
	r, err := f.ResolveImage("/6.5.30/dist/sa")
	if err != nil {
		t.Fatalf("ResolveImage: %v", err)
	}
	if r.Image != "base" || r.Path != "dist/sa" {
		t.Errorf("ResolveImage = %+v, want layer base, path dist/sa", r)
	}
	if _, err := f.ResolveImage("install.cmds"); err == nil {
		t.Error("a generated file resolved to a backing image")
	}
}
