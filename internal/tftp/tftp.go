// Package tftp is a read-only RFC 1350 TFTP server shaped for SGI PROMs:
// 512-byte blocks by default (RFC 2347/2348/2349 options when a client
// negotiates them), transfer sockets bound inside a configurable low port
// range (PROMs ignore transfers from high source ports), block counter
// rollover for files past 32MB, and tolerance for paths with or without a
// leading slash.
package tftp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned by a FileSystem when the path does not exist.
var ErrNotFound = errors.New("file not found")

// FileSystem is the tree the server reads from.
type FileSystem interface {
	Open(path string) (File, error)
}

// File is an open, random-access file.
type File interface {
	io.ReaderAt
	Size() int64
}

const (
	opRRQ   = 1
	opWRQ   = 2
	opDATA  = 3
	opACK   = 4
	opERROR = 5
	opOACK  = 6

	blockSize = 512 // default when a client doesn't negotiate blksize

	// minOptBlockSize and maxOptBlockSize are RFC 2348's valid blksize
	// range; a requested value outside it is clamped, not rejected.
	minOptBlockSize = 8
	maxOptBlockSize = 65464
)

// TFTP error codes.
const (
	errNotDefined      = 0
	errFileNotFound    = 1
	errAccessViolation = 2
)

// Server serves read requests. The zero value is not usable without FS.
type Server struct {
	FS FileSystem

	// AllowIP filters clients; nil allows everyone.
	AllowIP func(netip.Addr) bool

	// PortMin and PortMax bound the transfer sockets' local ports.
	// Zero means ephemeral.
	PortMin, PortMax int

	// RetryInterval is the DATA retransmit interval (default 1s).
	RetryInterval time.Duration

	// Retries is how many times a DATA block is resent before the
	// transfer is abandoned (default 5).
	Retries int

	// FinalRetries and FinalTimeout govern only the last DATA block of a
	// transfer (default 1 retry, 300ms). SGI PROMs reopen a file to seek
	// within it and routinely stop listening once they have what they
	// need, without ACKing the last block sent; the ordinary mid-transfer
	// retry budget (Retries x RetryInterval) exists for real packet loss,
	// not for that expected silence, so the last block gets a cheaper
	// policy to avoid paying it on every reopen.
	FinalRetries int
	FinalTimeout time.Duration

	// Logf, when set, receives one line per request and transfer event.
	Logf func(format string, args ...any)

	// Verbose logs every datagram arrival, including ones ignored.
	Verbose bool
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Serve answers requests arriving on pc (conventionally bound to :69)
// until pc is closed.
func (s *Server) Serve(pc net.PacketConn) error {
	buf := make([]byte, 2048)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		go s.handle(pc, addr, pkt)
	}
}

func (s *Server) handle(pc net.PacketConn, addr net.Addr, pkt []byte) {
	if s.Verbose {
		op := uint16(0)
		if len(pkt) >= 2 {
			op = binary.BigEndian.Uint16(pkt[0:2])
		}
		s.logf("tftp: recv %d bytes from %s (opcode %d)", len(pkt), addr, op)
	}
	if len(pkt) < 2 {
		return
	}
	udp, ok := addr.(*net.UDPAddr)
	if !ok {
		return
	}
	if s.AllowIP != nil {
		ip, _ := netip.AddrFromSlice(udp.IP)
		if !s.AllowIP(ip.Unmap()) {
			s.logf("tftp: %s: denied by client filter", addr)
			s.sendError(pc, addr, errAccessViolation, "access denied")
			return
		}
	}
	op := binary.BigEndian.Uint16(pkt[0:2])
	switch op {
	case opRRQ:
		file, mode, trailing, err := parseRRQ(pkt)
		if err != nil {
			s.sendError(pc, addr, errNotDefined, err.Error())
			return
		}
		// trailing is either RFC 2347 options or an SGI PROM's uninitialized
		// request buffer; parseOptions tells them apart.
		opts, ok := parseOptions(trailing)
		if !ok {
			opts = nil
		}
		if s.Verbose {
			extra := ""
			if len(trailing) > 0 {
				extra = fmt.Sprintf(" trailing=% x", trailing)
			}
			if len(opts) > 0 {
				extra += fmt.Sprintf(" opts=%v", opts)
			}
			s.logf("tftp: %s: RRQ file=%q mode=%q%s", addr, file, mode, extra)
		}
		s.serveFile(udp, file, mode, opts)
	case opWRQ:
		s.logf("tftp: %s: write refused", addr)
		s.sendError(pc, addr, errAccessViolation, "server is read-only")
	default:
		// stray DATA/ACK on the request port: ignore
	}
}

// parseRRQ splits a read request into filename, mode, and any trailing
// bytes after the mode (TFTP options, or junk a PROM left in its buffer).
func parseRRQ(pkt []byte) (file, mode string, trailing []byte, err error) {
	body := pkt[2:]
	i := bytes.IndexByte(body, 0)
	if i < 0 {
		return "", "", nil, fmt.Errorf("malformed request: no filename terminator")
	}
	file = string(body[:i])
	rest := body[i+1:]
	j := bytes.IndexByte(rest, 0)
	if j < 0 {
		return "", "", nil, fmt.Errorf("malformed request: no mode terminator")
	}
	mode = strings.ToLower(string(rest[:j]))
	// Not trimmed: option pairs are name\0value\0, so a trailing NUL is
	// structural, and parseOptions needs the exact bytes to tell a real
	// option list from an SGI PROM's uninitialized buffer junk.
	trailing = rest[j+1:]
	return file, mode, trailing, nil
}

// parseOptions interprets the bytes trailing an RRQ's mode string as RFC
// 2347 TFTP options: complete NAME\0VALUE\0 pairs, both printable ASCII.
// It returns ok=false for anything else, including the junk an SGI PROM's
// uninitialized request buffer leaves there (real captures: 9f c5 d2 00,
// or leftover fragments like "stal" from a prior request's filename) --
// that junk is neither reliably printable nor paired, so it never gets
// mistaken for an option.
func parseOptions(trailing []byte) (opts map[string]string, ok bool) {
	if len(trailing) == 0 {
		return nil, true
	}
	fields := bytes.Split(trailing, []byte{0})
	// A well-formed "name\0value\0..." splits into an even number of
	// fields plus one empty field from the final terminator.
	if len(fields) < 3 || len(fields[len(fields)-1]) != 0 {
		return nil, false
	}
	fields = fields[:len(fields)-1]
	if len(fields)%2 != 0 {
		return nil, false
	}
	opts = make(map[string]string, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		name, val := fields[i], fields[i+1]
		if len(name) == 0 || len(val) == 0 || !isPrintableASCII(name) || !isPrintableASCII(val) {
			return nil, false
		}
		opts[strings.ToLower(string(name))] = string(val)
	}
	return opts, true
}

func isPrintableASCII(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// negotiateOptions turns a parsed option set into the OACK body to send
// (nil if none is accepted) and the block size / per-block timeout this
// transfer should use. Only blksize (RFC 2348), timeout, and tsize (RFC
// 2349) are understood. Anything else -- including an RFC 7440 windowsize
// -- is left out of the OACK, which by RFC 2347 means "not negotiated":
// exactly today's classic one-block-per-ACK behavior for that option, so
// an unsupported option degrades safely instead of failing the transfer.
// A malformed value for a known option is likewise just dropped rather
// than erroring the whole negotiation.
func negotiateOptions(opts map[string]string, fileSize int64, defaultBlockSize int, defaultTimeout time.Duration) (oack []byte, blkSize int, timeout time.Duration) {
	blkSize = defaultBlockSize
	timeout = defaultTimeout
	if len(opts) == 0 {
		return nil, blkSize, timeout
	}
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint16(opOACK))
	accepted := false
	if v, ok := opts["blksize"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			switch {
			case n < minOptBlockSize:
				n = minOptBlockSize
			case n > maxOptBlockSize:
				n = maxOptBlockSize
			}
			blkSize = n
			fmt.Fprintf(&b, "blksize\x00%d\x00", n)
			accepted = true
		}
	}
	if v, ok := opts["timeout"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 255 {
			timeout = time.Duration(n) * time.Second
			fmt.Fprintf(&b, "timeout\x00%d\x00", n)
			accepted = true
		}
	}
	if _, ok := opts["tsize"]; ok {
		fmt.Fprintf(&b, "tsize\x00%d\x00", fileSize)
		accepted = true
	}
	if !accepted {
		return nil, defaultBlockSize, defaultTimeout
	}
	return b.Bytes(), blkSize, timeout
}

func (s *Server) sendError(pc net.PacketConn, addr net.Addr, code uint16, msg string) {
	b := make([]byte, 4+len(msg)+1)
	binary.BigEndian.PutUint16(b[0:], opERROR)
	binary.BigEndian.PutUint16(b[2:], code)
	copy(b[4:], msg)
	pc.WriteTo(b, addr)
}

// listenTransfer binds the per-transfer socket inside the configured
// port range. It returns a concrete *net.UDPConn, not net.PacketConn, so
// the transfer loop can use the AddrPort-based read/write calls: the
// net.Addr-returning ones allocate a new *net.UDPAddr on every packet.
func (s *Server) listenTransfer() (*net.UDPConn, error) {
	if s.PortMin == 0 && s.PortMax == 0 {
		return net.ListenUDP("udp", nil)
	}
	for p := s.PortMin; p <= s.PortMax; p++ {
		pc, err := net.ListenUDP("udp", &net.UDPAddr{Port: p})
		if err == nil {
			return pc, nil
		}
	}
	return nil, fmt.Errorf("no free port in [%d,%d]", s.PortMin, s.PortMax)
}

// sendAndWaitAck sends pkt up to retries+1 times, spaced by timeout, until
// an ACK for wantBlock arrives from client (block 0 acknowledges an OACK).
// acked reports success; aborted reports the send itself failing or the
// client sending a TFTP ERROR -- both end the transfer immediately, with
// no further attempts. ackbuf is caller-owned and reused across calls so a
// transfer allocates it once, not per block; clientAP is client's address
// pre-converted so the hot loop never does it. Uses the AddrPort-flavored
// UDPConn calls, not ReadFrom/WriteTo: those return/take a net.Addr
// interface and allocate a new *net.UDPAddr for every single packet.
func (s *Server) sendAndWaitAck(pc *net.UDPConn, client *net.UDPAddr, clientAP netip.AddrPort, ackbuf, pkt []byte, wantBlock uint16, timeout time.Duration, retries int) (acked, aborted bool) {
	for attempt := 0; attempt <= retries; attempt++ {
		if _, err := pc.WriteToUDPAddrPort(pkt, clientAP); err != nil {
			s.logf("tftp: %s: send: %v", client, err)
			return false, true
		}
		if s.Verbose && attempt > 0 {
			s.logf("tftp: %s: resend block %d (attempt %d)", client, wantBlock, attempt)
		}
		pc.SetReadDeadline(time.Now().Add(timeout))
		for {
			n, from, err := pc.ReadFromUDPAddrPort(ackbuf)
			if err != nil {
				break // timeout: resend
			}
			if from.Addr().Unmap() != clientAP.Addr() || from.Port() != clientAP.Port() {
				if s.Verbose {
					s.logf("tftp: %s: stray packet from %s, ignoring", client, from)
				}
				continue // stray packet from elsewhere
			}
			if n < 4 {
				continue
			}
			switch binary.BigEndian.Uint16(ackbuf[0:2]) {
			case opACK:
				got := binary.BigEndian.Uint16(ackbuf[2:4])
				if got == wantBlock {
					if s.Verbose {
						s.logf("tftp: %s: ACK block %d", client, got)
					}
					return true, false
				}
				if s.Verbose {
					s.logf("tftp: %s: ACK block %d (waiting for %d)", client, got, wantBlock)
				}
			case opERROR:
				s.logf("tftp: %s: client ERROR code %d: %q", client, binary.BigEndian.Uint16(ackbuf[2:4]), ackbuf[4:n])
				return false, true
			default:
				if s.Verbose {
					s.logf("tftp: %s: unexpected opcode %d during transfer", client, binary.BigEndian.Uint16(ackbuf[0:2]))
				}
			}
		}
	}
	return false, false
}

func (s *Server) serveFile(client *net.UDPAddr, name, mode string, opts map[string]string) {
	// PROMs disagree about the leading slash; accept both.
	path := strings.TrimPrefix(name, "/")
	f, err := s.FS.Open(path)
	if err != nil {
		s.logf("tftp: %s: %q: %v", client, name, err)
		pc, lerr := s.listenTransfer()
		if lerr != nil {
			return
		}
		defer pc.Close()
		code := uint16(errNotDefined)
		if errors.Is(err, ErrNotFound) {
			code = errFileNotFound
		}
		s.sendError(pc, client, code, err.Error())
		return
	}
	if mode != "octet" && mode != "netascii" {
		s.logf("tftp: %s: %q: unknown mode %q", client, name, mode)
	}
	pc, err := s.listenTransfer()
	if err != nil {
		s.logf("tftp: %s: %v", client, err)
		return
	}
	defer pc.Close()

	retry := s.RetryInterval
	if retry == 0 {
		retry = time.Second
	}
	retries := s.Retries
	if retries == 0 {
		retries = 5
	}

	size := f.Size()
	oack, effBlockSize, effRetry := negotiateOptions(opts, size, blockSize, retry)
	retry = effRetry

	finalRetries := s.FinalRetries
	if finalRetries == 0 {
		finalRetries = 1
	}
	finalTimeout := s.FinalTimeout
	if finalTimeout == 0 {
		finalTimeout = 300 * time.Millisecond
	}
	if finalTimeout > retry {
		finalTimeout = retry
	}

	s.logf("tftp: %s: sending %q (%d bytes) from port %d",
		client, name, size, pc.LocalAddr().(*net.UDPAddr).Port)

	// Pre-converted once per transfer; Unmap keeps a v4-mapped v6 address
	// comparing equal to its plain v4 form on the hot path below.
	rawAP := client.AddrPort()
	clientAP := netip.AddrPortFrom(rawAP.Addr().Unmap(), rawAP.Port())

	data := make([]byte, 4+effBlockSize)
	ack := make([]byte, 1500)

	if oack != nil {
		if s.Verbose {
			s.logf("tftp: %s: OACK %v", client, opts)
		}
		acked, aborted := s.sendAndWaitAck(pc, client, clientAP, ack, oack, 0, retry, retries)
		if aborted {
			return
		}
		if !acked {
			s.logf("tftp: %s: %q: no ACK for OACK, giving up", client, name)
			return
		}
	}

	// block numbers start at 1 and wrap around 65535 for large files
	var block uint16 = 1
	for off := int64(0); ; off += int64(effBlockSize) {
		want := size - off
		if want > int64(effBlockSize) {
			want = int64(effBlockSize)
		}
		if want < 0 {
			want = 0
		}
		binary.BigEndian.PutUint16(data[0:], opDATA)
		binary.BigEndian.PutUint16(data[2:], block)
		if want > 0 {
			if _, err := f.ReadAt(data[4:4+want], off); err != nil && err != io.EOF {
				s.logf("tftp: %s: read %q at %d: %v", client, name, off, err)
				s.sendError(pc, client, errNotDefined, "read error")
				return
			}
		}
		pkt := data[:4+want]
		if s.Verbose {
			s.logf("tftp: %s: DATA block %d (%d bytes, off %d)", client, block, want, off)
		}

		// The last DATA block of a transfer routinely goes unacknowledged:
		// an SGI PROM reopens the file to seek and stops listening once it
		// has what it needs. Give up on it cheaply instead of paying the
		// mid-transfer retry budget on every reopen.
		isFinal := want < int64(effBlockSize)
		blockRetries, blockTimeout := retries, retry
		if isFinal {
			blockRetries, blockTimeout = finalRetries, finalTimeout
		}

		acked, aborted := s.sendAndWaitAck(pc, client, clientAP, ack, pkt, block, blockTimeout, blockRetries)
		if aborted {
			return
		}
		if !acked {
			s.logf("tftp: %s: %q: no ACK for block %d, giving up", client, name, block)
			return
		}
		if isFinal {
			s.logf("tftp: %s: %q done", client, name)
			return
		}
		block++
	}
}
