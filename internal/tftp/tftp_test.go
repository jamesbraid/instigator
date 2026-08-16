package tftp

import (
	"bytes"
	"encoding/binary"
	"fmt"
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

// rrqWithOptions builds an RRQ carrying RFC 2347 options after the mode
// string, name\0value\0 per pair, exactly as a negotiating client sends.
func rrqWithOptions(file, mode string, opts [][2]string) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint16(1))
	b.WriteString(file)
	b.WriteByte(0)
	b.WriteString(mode)
	b.WriteByte(0)
	for _, kv := range opts {
		b.WriteString(kv[0])
		b.WriteByte(0)
		b.WriteString(kv[1])
		b.WriteByte(0)
	}
	return b.Bytes()
}

// parseOACKBody decodes an OACK packet's name\0value\0 pairs.
func parseOACKBody(pkt []byte) map[string]string {
	m := map[string]string{}
	fields := bytes.Split(bytes.TrimRight(pkt[2:], "\x00"), []byte{0})
	for i := 0; i+1 < len(fields); i += 2 {
		m[strings.ToLower(string(fields[i]))] = string(fields[i+1])
	}
	return m
}

// fetchWithOptions runs a full option-negotiating transfer: it sends an
// RRQ with opts, ACKs an OACK (block 0) if one comes back, then reads DATA
// using whatever blksize was negotiated (or 512 if none was). It returns
// the reassembled content and the negotiated options (nil if the server
// never sent an OACK).
func fetchWithOptions(t *testing.T, addr *net.UDPAddr, file string, opts [][2]string) ([]byte, map[string]string) {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.WriteTo(rrqWithOptions(file, "octet", opts), addr); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 66000)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, from, err := c.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}

	var negotiated map[string]string
	blkSize := 512
	if binary.BigEndian.Uint16(buf[0:2]) == 6 { // OACK
		negotiated = parseOACKBody(buf[:n])
		if v, ok := negotiated["blksize"]; ok {
			fmt.Sscanf(v, "%d", &blkSize)
		}
		ack := make([]byte, 4)
		binary.BigEndian.PutUint16(ack[0:], 4)
		binary.BigEndian.PutUint16(ack[2:], 0)
		if _, err := c.WriteTo(ack, from); err != nil {
			t.Fatal(err)
		}
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if n, from, err = c.ReadFrom(buf); err != nil {
			t.Fatal(err)
		}
	}

	var out []byte
	for {
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
		if n-4 < blkSize {
			break
		}
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if n, from, err = c.ReadFrom(buf); err != nil {
			t.Fatal(err)
		}
	}
	return out, negotiated
}

func TestOptionsNegotiationBlksize(t *testing.T) {
	// several negotiated-size blocks, not a multiple of 1024, to exercise
	// full blocks and a short final block
	content := bytes.Repeat([]byte("q"), 3000)
	addr := startServer(t, &Server{FS: memFS{"f": content}})
	got, negotiated := fetchWithOptions(t, addr, "f", [][2]string{{"blksize", "1024"}})
	if !bytes.Equal(got, content) {
		t.Fatalf("got %d bytes, want %d", len(got), len(content))
	}
	if negotiated == nil {
		t.Fatal("server did not OACK the blksize option")
	}
	if negotiated["blksize"] != "1024" {
		t.Fatalf("negotiated blksize = %q, want 1024", negotiated["blksize"])
	}
}

func TestOptionsNegotiationTsizeAndTimeout(t *testing.T) {
	content := bytes.Repeat([]byte("q"), 100)
	addr := startServer(t, &Server{FS: memFS{"f": content}})
	got, negotiated := fetchWithOptions(t, addr, "f", [][2]string{{"tsize", "0"}, {"timeout", "3"}})
	if !bytes.Equal(got, content) {
		t.Fatalf("got %d bytes, want %d", len(got), len(content))
	}
	if negotiated == nil {
		t.Fatal("server did not OACK tsize/timeout")
	}
	if want := fmt.Sprint(len(content)); negotiated["tsize"] != want {
		t.Fatalf("negotiated tsize = %q, want %q", negotiated["tsize"], want)
	}
	if negotiated["timeout"] != "3" {
		t.Fatalf("negotiated timeout = %q, want 3", negotiated["timeout"])
	}
}

func TestOptionsBlksizeClampedToRFCMax(t *testing.T) {
	content := []byte("small")
	addr := startServer(t, &Server{FS: memFS{"f": content}})
	got, negotiated := fetchWithOptions(t, addr, "f", [][2]string{{"blksize", "9999999"}})
	if !bytes.Equal(got, content) {
		t.Fatalf("got %q, want %q", got, content)
	}
	if negotiated["blksize"] != "65464" {
		t.Fatalf("negotiated blksize = %q, want clamped to 65464 (RFC 2348 max)", negotiated["blksize"])
	}
}

func TestOptionsUnrecognizedNameIgnored(t *testing.T) {
	// windowsize (RFC 7440) is well-formed but unimplemented: per RFC 2347
	// an option missing from the OACK means "not negotiated", so the
	// transfer must still complete correctly at the default block size,
	// with no OACK if nothing else was requested.
	content := bytes.Repeat([]byte("w"), 600)
	addr := startServer(t, &Server{FS: memFS{"f": content}})
	got, negotiated := fetchWithOptions(t, addr, "f", [][2]string{{"windowsize", "4"}})
	if !bytes.Equal(got, content) {
		t.Fatalf("got %d bytes, want %d", len(got), len(content))
	}
	if negotiated != nil {
		t.Fatalf("server OACKed an unsupported option: %v", negotiated)
	}
}

// TestPROMJunkNeverNegotiatesOptions guards the live SGI Octane path: the
// PROM leaves uninitialized-buffer junk after "octet\0" (real capture:
// 9f c5 d2 00) that must never be mistaken for RFC 2347 options.
func TestPROMJunkNeverNegotiatesOptions(t *testing.T) {
	content := []byte("fx dummy content")
	addr := startServer(t, &Server{FS: memFS{"f": content}})
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint16(1))
	b.WriteString("f")
	b.WriteByte(0)
	b.WriteString("octet")
	b.WriteByte(0)
	b.Write([]byte{0x9f, 0xc5, 0xd2, 0x00}) // captured PROM buffer junk
	if _, err := c.WriteTo(b.Bytes(), addr); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 1500)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := c.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if op := binary.BigEndian.Uint16(buf[0:2]); op != 3 {
		t.Fatalf("opcode %d, want DATA (junk must never trigger an OACK)", op)
	}
	if block := binary.BigEndian.Uint16(buf[2:4]); block != 1 {
		t.Fatalf("block %d, want 1", block)
	}
	if !bytes.Equal(buf[4:n], content) {
		t.Fatalf("content mismatch: got %q, want %q", buf[4:n], content)
	}
}

// TestFinalBlockGivesUpQuickly is the end-of-transfer case a real Octane2
// PROM produces constantly: it reopens mid-file to seek, so a transfer's
// last DATA block frequently goes unacknowledged on purpose. The server
// must not burn the full mid-transfer retry budget (5 x 1s) finding that
// out.
func TestFinalBlockGivesUpQuickly(t *testing.T) {
	content := bytes.Repeat([]byte("z"), 100) // one block: block 1 is final
	addr := startServer(t, &Server{FS: memFS{"f": content}})
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.WriteTo(rrq("f", "octet"), addr); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 1500)
	var lastRecv time.Time
	windowEnd := time.Now().Add(2500 * time.Millisecond)
	for {
		if time.Until(windowEnd) <= 0 {
			break
		}
		c.SetReadDeadline(windowEnd)
		_, _, err := c.ReadFrom(buf)
		if err != nil {
			break // no more packets before the window closes
		}
		if op := binary.BigEndian.Uint16(buf[0:2]); op != 3 {
			t.Fatalf("opcode %d, want DATA", op)
		}
		lastRecv = time.Now()
		// deliberately never ACK: this is the reopen-and-stop-listening case
	}
	if lastRecv.IsZero() {
		t.Fatal("never received the final DATA block")
	}
	if silence := windowEnd.Sub(lastRecv); silence < time.Second {
		t.Fatalf("server was still retrying %s before the 2.5s window closed; "+
			"the final block should give up well inside the old 5s budget", silence)
	}
}

// drain runs a full transfer like fetch but discards content instead of
// accumulating it, so a benchmark measures roughly the server's cost, not
// a growing client-side slice.
func drain(tb testing.TB, addr *net.UDPAddr, file string, wantLen int) {
	tb.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	defer c.Close()
	if _, err := c.WriteTo(rrq(file, "octet"), addr); err != nil {
		tb.Fatal(err)
	}
	buf := make([]byte, 1500)
	ack := make([]byte, 4)
	got := 0
	for {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, from, err := c.ReadFrom(buf)
		if err != nil {
			tb.Fatal(err)
		}
		block := binary.BigEndian.Uint16(buf[2:4])
		got += n - 4
		binary.BigEndian.PutUint16(ack[0:], 4)
		binary.BigEndian.PutUint16(ack[2:], block)
		if _, err := c.WriteTo(ack, from); err != nil {
			tb.Fatal(err)
		}
		if n-4 < 512 {
			break
		}
	}
	if got != wantLen {
		tb.Fatalf("got %d bytes, want %d", got, wantLen)
	}
}

// BenchmarkTransfer measures per-transfer cost of a non-verbose 256KiB
// fetch (~512 blocks at the default block size), to catch any accidental
// per-block allocation or syscall overhead creeping into the hot path.
func BenchmarkTransfer(b *testing.B) {
	content := bytes.Repeat([]byte("x"), 256*1024)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer pc.Close()
	s := &Server{FS: memFS{"f": content}}
	go s.Serve(pc)
	addr := pc.LocalAddr().(*net.UDPAddr)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drain(b, addr, "f", len(content))
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
