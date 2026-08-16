// Package tftp is a read-only RFC 1350 TFTP server shaped for SGI PROMs:
// fixed 512-byte blocks, transfer sockets bound inside a configurable
// low port range (PROMs ignore transfers from high source ports), block
// counter rollover for files past 32MB, and tolerance for paths with or
// without a leading slash.
package tftp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
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

	blockSize = 512
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
		file, mode, opts, err := parseRRQ(pkt)
		if err != nil {
			s.sendError(pc, addr, errNotDefined, err.Error())
			return
		}
		if s.Verbose {
			extra := ""
			if len(opts) > 0 {
				// SGI PROMs append junk (or options) after the mode; show
				// it as hex so an option negotiation is never invisible.
				extra = fmt.Sprintf(" trailing=% x", opts)
			}
			s.logf("tftp: %s: RRQ file=%q mode=%q%s", addr, file, mode, extra)
		}
		s.serveFile(udp, file, mode)
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
	trailing = bytes.TrimRight(rest[j+1:], "\x00")
	return file, mode, trailing, nil
}

func (s *Server) sendError(pc net.PacketConn, addr net.Addr, code uint16, msg string) {
	b := make([]byte, 4+len(msg)+1)
	binary.BigEndian.PutUint16(b[0:], opERROR)
	binary.BigEndian.PutUint16(b[2:], code)
	copy(b[4:], msg)
	pc.WriteTo(b, addr)
}

// listenTransfer binds the per-transfer socket inside the configured
// port range.
func (s *Server) listenTransfer() (net.PacketConn, error) {
	if s.PortMin == 0 && s.PortMax == 0 {
		return net.ListenPacket("udp", ":0")
	}
	for p := s.PortMin; p <= s.PortMax; p++ {
		pc, err := net.ListenPacket("udp", fmt.Sprintf(":%d", p))
		if err == nil {
			return pc, nil
		}
	}
	return nil, fmt.Errorf("no free port in [%d,%d]", s.PortMin, s.PortMax)
}

func (s *Server) serveFile(client *net.UDPAddr, name, mode string) {
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

	s.logf("tftp: %s: sending %q (%d bytes) from port %d",
		client, name, f.Size(), pc.LocalAddr().(*net.UDPAddr).Port)

	size := f.Size()
	data := make([]byte, 4+blockSize)
	ack := make([]byte, 1500)
	// block numbers start at 1 and wrap around 65535 for large files
	var block uint16 = 1
	for off := int64(0); ; off += blockSize {
		want := size - off
		if want > blockSize {
			want = blockSize
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

		acked := false
		for attempt := 0; attempt <= retries; attempt++ {
			if _, err := pc.WriteTo(pkt, client); err != nil {
				s.logf("tftp: %s: send: %v", client, err)
				return
			}
			if s.Verbose {
				if attempt == 0 {
					s.logf("tftp: %s: DATA block %d (%d bytes, off %d)", client, block, want, off)
				} else {
					s.logf("tftp: %s: resend block %d (attempt %d)", client, block, attempt)
				}
			}
			pc.SetReadDeadline(time.Now().Add(retry))
			for {
				n, from, err := pc.ReadFrom(ack)
				if err != nil {
					break // timeout: resend
				}
				fu, ok := from.(*net.UDPAddr)
				if !ok || !fu.IP.Equal(client.IP) || fu.Port != client.Port {
					if s.Verbose {
						s.logf("tftp: %s: stray packet from %s, ignoring", client, from)
					}
					continue // stray packet from elsewhere
				}
				if n < 4 {
					continue
				}
				switch binary.BigEndian.Uint16(ack[0:2]) {
				case opACK:
					got := binary.BigEndian.Uint16(ack[2:4])
					acked = got == block
					if s.Verbose {
						if acked {
							s.logf("tftp: %s: ACK block %d", client, got)
						} else {
							s.logf("tftp: %s: ACK block %d (waiting for %d)", client, got, block)
						}
					}
				case opERROR:
					s.logf("tftp: %s: client ERROR code %d: %q", client, binary.BigEndian.Uint16(ack[2:4]), ack[4:n])
					return
				default:
					if s.Verbose {
						s.logf("tftp: %s: unexpected opcode %d during transfer", client, binary.BigEndian.Uint16(ack[0:2]))
					}
				}
				if acked {
					break
				}
			}
			if acked {
				break
			}
		}
		if !acked {
			s.logf("tftp: %s: %q: no ACK for block %d, giving up", client, name, block)
			return
		}
		if want < blockSize {
			s.logf("tftp: %s: %q done", client, name)
			return
		}
		block++
	}
}
