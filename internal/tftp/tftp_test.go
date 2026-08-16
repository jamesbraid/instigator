package tftp

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// memFS serves in-memory files.
type memFS map[string][]byte

func (m memFS) Open(path string) (File, error) {
	b, ok := m[path]
	if !ok {
		return nil, ErrNotFound
	}
	return memFile{bytes.NewReader(b), int64(len(b))}, nil
}

type memFile struct {
	*bytes.Reader
	size int64
}

func (f memFile) Size() int64 { return f.size }

func startServer(t *testing.T, s *Server) *net.UDPAddr {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go s.Serve(pc)
	return pc.LocalAddr().(*net.UDPAddr)
}

func rrq(file, mode string) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint16(1))
	b.WriteString(file)
	b.WriteByte(0)
	b.WriteString(mode)
	b.WriteByte(0)
	return b.Bytes()
}

// fetch runs a full client-side transfer and returns content and the
// server's transfer port.
func fetch(t *testing.T, addr *net.UDPAddr, file string) ([]byte, int) {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.WriteTo(rrq(file, "octet"), addr); err != nil {
		t.Fatal(err)
	}
	var out []byte
	buf := make([]byte, 1500)
	var from net.Addr
	for {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, f, err := c.ReadFrom(buf)
		if err != nil {
			t.Fatal(err)
		}
		from = f
		op := binary.BigEndian.Uint16(buf[0:2])
		if op == 5 {
			t.Fatalf("server error: %q", buf[4:n])
		}
		if op != 3 {
			t.Fatalf("unexpected opcode %d", op)
		}
		block := binary.BigEndian.Uint16(buf[2:4])
		out = append(out, buf[4:n]...)
		ack := make([]byte, 4)
		binary.BigEndian.PutUint16(ack[0:], 4)
		binary.BigEndian.PutUint16(ack[2:], block)
		if _, err := c.WriteTo(ack, from); err != nil {
			t.Fatal(err)
		}
		if n-4 < 512 {
			break
		}
	}
	return out, from.(*net.UDPAddr).Port
}

func TestServesSmallFile(t *testing.T) {
	content := []byte("fx dummy content")
	addr := startServer(t, &Server{FS: memFS{"stand/fx.64": content}})
	got, _ := fetch(t, addr, "stand/fx.64")
	if !bytes.Equal(got, content) {
		t.Fatalf("got %q", got)
	}
}

func TestStripsLeadingSlash(t *testing.T) {
	content := []byte("x")
	addr := startServer(t, &Server{FS: memFS{"stand/fx.64": content}})
	got, _ := fetch(t, addr, "/stand/fx.64")
	if !bytes.Equal(got, content) {
		t.Fatalf("got %q", got)
	}
}

func TestMultiBlockAndExactMultiple(t *testing.T) {
	// 1024 bytes: exactly two full blocks, must be terminated by an
	// empty third DATA block
	content := bytes.Repeat([]byte("z"), 1024)
	addr := startServer(t, &Server{FS: memFS{"f": content}})
	got, _ := fetch(t, addr, "f")
	if !bytes.Equal(got, content) {
		t.Fatalf("got %d bytes, want %d", len(got), len(content))
	}
}

func TestTransferPortRange(t *testing.T) {
	addr := startServer(t, &Server{FS: memFS{"f": []byte("y")}, PortMin: 3000, PortMax: 3010})
	_, port := fetch(t, addr, "f")
	if port < 3000 || port > 3010 {
		t.Fatalf("transfer port %d outside [3000,3010]", port)
	}
}

func TestUnknownFileErrors(t *testing.T) {
	addr := startServer(t, &Server{FS: memFS{}})
	c, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer c.Close()
	c.WriteTo(rrq("absent", "octet"), addr)
	buf := make([]byte, 1500)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := c.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if op := binary.BigEndian.Uint16(buf[0:2]); op != 5 {
		t.Fatalf("opcode %d, want ERROR", op)
	}
	if !strings.Contains(string(buf[4:n]), "not found") {
		t.Fatalf("error text %q", buf[4:n])
	}
}

func TestWriteRefused(t *testing.T) {
	addr := startServer(t, &Server{FS: memFS{}})
	c, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer c.Close()
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint16(2)) // WRQ
	b.WriteString("f")
	b.WriteByte(0)
	b.WriteString("octet")
	b.WriteByte(0)
	c.WriteTo(b.Bytes(), addr)
	buf := make([]byte, 1500)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := c.ReadFrom(buf); err != nil {
		t.Fatal(err)
	}
	if op := binary.BigEndian.Uint16(buf[0:2]); op != 5 {
		t.Fatalf("opcode %d, want ERROR", op)
	}
}

func TestClientFilter(t *testing.T) {
	addr := startServer(t, &Server{
		FS:      memFS{"f": []byte("y")},
		AllowIP: func(a netip.Addr) bool { return false },
	})
	c, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer c.Close()
	c.WriteTo(rrq("f", "octet"), addr)
	buf := make([]byte, 1500)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := c.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if op := binary.BigEndian.Uint16(buf[0:2]); op != 5 {
		t.Fatalf("opcode %d, want ERROR (access violation), got %q", op, buf[:n])
	}
}

func TestBlockCounterRollover(t *testing.T) {
	if testing.Short() {
		t.Skip("33MB transfer; run without -short")
	}
	// 33MB crosses the 16-bit block counter: 512-byte blocks wrap at
	// 65535, which real miniroot kernels can reach
	content := bytes.Repeat([]byte("R"), 33*1024*1024)
	content[len(content)-1] = 'E'
	addr := startServer(t, &Server{FS: memFS{"mr": content}})
	got, _ := fetch(t, addr, "mr")
	if len(got) != len(content) {
		t.Fatalf("got %d bytes, want %d", len(got), len(content))
	}
	if got[len(got)-1] != 'E' {
		t.Fatal("tail corrupted")
	}
}

func TestRetransmitOnLostAck(t *testing.T) {
	content := bytes.Repeat([]byte("r"), 700) // two blocks
	addr := startServer(t, &Server{FS: memFS{"f": content}, RetryInterval: 50 * time.Millisecond})
	c, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer c.Close()
	c.WriteTo(rrq("f", "octet"), addr)
	buf := make([]byte, 1500)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n1, from, err := c.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	first := append([]byte{}, buf[:n1]...)
	// don't ACK; the server must resend the same DATA block
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n2, _, err := c.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, buf[:n2]) {
		t.Fatalf("retransmit differs: %d vs %d bytes", n1, n2)
	}
	_ = from
}
