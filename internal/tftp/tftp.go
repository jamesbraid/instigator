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
	"sync"
	"time"

	"github.com/jamesbraid/instigator/internal/capture"
	"github.com/jamesbraid/instigator/internal/logging"
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

// Resolved is what ImageResolver reports: the backing image's filename and
// the location within that image's own filesystem.
type Resolved struct {
	Image string
	Path  string
}

// ImageResolver is optionally implemented by a FileSystem that can say
// which image file and in-image path a served path came from, so a
// recorded transfer names the backing image. A FileSystem that doesn't
// back onto named media need not implement it.
type ImageResolver interface {
	ResolveImage(path string) (Resolved, error)
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

	// Logger, when set, receives leveled log output: DEBUG for every
	// datagram and per-block DATA/ACK detail, INFO for each transfer
	// started or finished, WARN for a request refused or a transfer a
	// client stopped acking, ERROR for this side failing to read or
	// send. A nil Logger is silent.
	Logger *logging.Logger

	// Recorder, when set, receives a tftp_transfer_end record for each
	// completed or abandoned transfer. A nil Recorder is a no-op.
	Recorder *capture.Recorder

	// ClientName, when set, maps a client IP to its configured alias for
	// the recorded transfer. Nil, or a miss, records the IP.
	ClientName func(netip.Addr) string

	mu     sync.Mutex
	closed bool                    // set by Shutdown; stop spawning new transfers
	xfers  map[net.PacketConn]bool // active transfer sockets, closed by Shutdown to cancel
	wg     sync.WaitGroup          // active transfer goroutines
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
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			continue
		}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			s.handle(pc, addr, pkt)
		}()
	}
}

// Shutdown stops accepting new transfers, closes every in-flight transfer
// socket so a transfer stuck in its retry budget fails its next send or
// read and aborts at once, and waits up to timeout for the handlers to
// finish recording. A bare Wait would not cancel a transfer, whose default
// retries can outlast the drain window. Returns false if the timeout still
// elapsed with transfers running.
func (s *Server) Shutdown(timeout time.Duration) bool {
	s.mu.Lock()
	s.closed = true
	for pc := range s.xfers {
		pc.Close()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// trackXfer registers a transfer socket so Shutdown can cancel it.
func (s *Server) trackXfer(pc net.PacketConn) {
	s.mu.Lock()
	if s.xfers == nil {
		s.xfers = make(map[net.PacketConn]bool)
	}
	s.xfers[pc] = true
	s.mu.Unlock()
}

func (s *Server) untrackXfer(pc net.PacketConn) {
	s.mu.Lock()
	delete(s.xfers, pc)
	s.mu.Unlock()
}

func (s *Server) handle(pc net.PacketConn, addr net.Addr, pkt []byte) {
	if s.Logger.Enabled(logging.LevelDebug) {
		op := uint16(0)
		if len(pkt) >= 2 {
			op = binary.BigEndian.Uint16(pkt[0:2])
		}
		s.Logger.Debugf("tftp: recv %d bytes from %s (opcode %d)", len(pkt), addr, op)
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
			s.Logger.Warnf("tftp: %s: denied by client filter", addr)
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
		if s.Logger.Enabled(logging.LevelDebug) {
			extra := ""
			if len(trailing) > 0 {
				extra = fmt.Sprintf(" trailing=% x", trailing)
			}
			if len(opts) > 0 {
				extra += fmt.Sprintf(" opts=%v", opts)
			}
			s.Logger.Debugf("tftp: %s: RRQ file=%q mode=%q%s", addr, file, mode, extra)
		}
		s.serveFile(udp, file, mode, opts)
	case opWRQ:
		s.Logger.Warnf("tftp: %s: write refused", addr)
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
// sendAndWaitAck sends pkt and waits for its ACK, resending up to retries
// times. resends is how many retransmits it took (0 if the first send was
// acked), so a transfer can report its total retry count.
func (s *Server) sendAndWaitAck(pc *net.UDPConn, client *net.UDPAddr, clientAP netip.AddrPort, ackbuf, pkt []byte, wantBlock uint16, timeout time.Duration, retries int) (acked, aborted bool, resends int) {
	for attempt := 0; attempt <= retries; attempt++ {
		if _, err := pc.WriteToUDPAddrPort(pkt, clientAP); err != nil {
			s.Logger.Errorf("tftp: %s: send: %v", client, err)
			return false, true, attempt
		}
		if attempt > 0 && s.Logger.Enabled(logging.LevelDebug) {
			s.Logger.Debugf("tftp: %s: resend block %d (attempt %d)", client, wantBlock, attempt)
		}
		pc.SetReadDeadline(time.Now().Add(timeout))
		for {
			n, from, err := pc.ReadFromUDPAddrPort(ackbuf)
			if err != nil {
				break // timeout: resend
			}
			if from.Addr().Unmap() != clientAP.Addr() || from.Port() != clientAP.Port() {
				if s.Logger.Enabled(logging.LevelDebug) {
					s.Logger.Debugf("tftp: %s: stray packet from %s, ignoring", client, from)
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
					if s.Logger.Enabled(logging.LevelDebug) {
						s.Logger.Debugf("tftp: %s: ACK block %d", client, got)
					}
					return true, false, attempt
				}
				if s.Logger.Enabled(logging.LevelDebug) {
					s.Logger.Debugf("tftp: %s: ACK block %d (waiting for %d)", client, got, wantBlock)
				}
			case opERROR:
				s.Logger.Warnf("tftp: %s: client ERROR code %d: %q", client, binary.BigEndian.Uint16(ackbuf[2:4]), ackbuf[4:n])
				return false, true, attempt
			default:
				if s.Logger.Enabled(logging.LevelDebug) {
					s.Logger.Debugf("tftp: %s: unexpected opcode %d during transfer", client, binary.BigEndian.Uint16(ackbuf[0:2]))
				}
			}
		}
	}
	return false, false, retries
}

func (s *Server) serveFile(client *net.UDPAddr, name, mode string, opts map[string]string) {
	// PROMs disagree about the leading slash; accept both.
	path := strings.TrimPrefix(name, "/")

	// Transfer accounting for the capture record. These are read only by
	// the deferred record below (registered only when capture is on), so
	// they stay in scope across every early return and pick up their final
	// values before it fires.
	start := time.Now()
	var (
		size             int64
		effBlockSize     = blockSize
		blocksSent       int
		bytesSent        int64 // put on the wire
		bytesAcked       int64 // confirmed by the client
		resendTotal      int
		result           = "error"
		image, imagePath string
	)
	if s.Recorder != nil {
		if ir, ok := s.FS.(ImageResolver); ok {
			if r, rerr := ir.ResolveImage(path); rerr == nil {
				image, imagePath = r.Image, r.Path
			}
		}
		clientID := client.IP.String()
		if s.ClientName != nil {
			if a, ok := netip.AddrFromSlice(client.IP); ok {
				if name := s.ClientName(a.Unmap()); name != "" {
					clientID = name
				}
			}
		}
		defer func() {
			s.Recorder.TFTPTransferEnd(capture.TransferRecord{
				Client:      clientID,
				Name:        name,
				TreePath:    path,
				Image:       image,
				ImagePath:   imagePath,
				Size:        size,
				BlockSize:   effBlockSize,
				Blocks:      blocksSent,
				BytesSent:   bytesSent,
				BytesAcked:  bytesAcked,
				Retransmits: resendTotal,
				DurationMS:  time.Since(start).Milliseconds(),
				Result:      result,
			})
		}()
	}

	f, err := s.FS.Open(path)
	if err != nil {
		s.Logger.Warnf("tftp: %s: %q: %v", client, name, err)
		pc, lerr := s.listenTransfer()
		if lerr != nil {
			return
		}
		defer pc.Close()
		code := uint16(errNotDefined)
		if errors.Is(err, ErrNotFound) {
			code = errFileNotFound
			result = "notfound"
		}
		s.sendError(pc, client, code, err.Error())
		return
	}
	if mode != "octet" && mode != "netascii" {
		s.Logger.Warnf("tftp: %s: %q: unknown mode %q", client, name, mode)
	}
	pc, err := s.listenTransfer()
	if err != nil {
		s.Logger.Errorf("tftp: %s: %v", client, err)
		return
	}
	defer pc.Close()
	// Register the transfer socket so a shutdown can cancel this transfer
	// rather than wait out its retry budget.
	s.trackXfer(pc)
	defer s.untrackXfer(pc)

	retry := s.RetryInterval
	if retry == 0 {
		retry = time.Second
	}
	retries := s.Retries
	if retries == 0 {
		retries = 5
	}

	size = f.Size()
	oack, negBlockSize, effRetry := negotiateOptions(opts, size, blockSize, retry)
	effBlockSize = negBlockSize
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

	s.Logger.Infof("tftp: %s: sending %q (%d bytes) from port %d",
		client, name, size, pc.LocalAddr().(*net.UDPAddr).Port)

	// Pre-converted once per transfer; Unmap keeps a v4-mapped v6 address
	// comparing equal to its plain v4 form on the hot path below.
	rawAP := client.AddrPort()
	clientAP := netip.AddrPortFrom(rawAP.Addr().Unmap(), rawAP.Port())

	data := make([]byte, 4+effBlockSize)
	ack := make([]byte, 1500)

	if oack != nil {
		if s.Logger.Enabled(logging.LevelDebug) {
			s.Logger.Debugf("tftp: %s: OACK %v", client, opts)
		}
		acked, aborted, resends := s.sendAndWaitAck(pc, client, clientAP, ack, oack, 0, retry, retries)
		resendTotal += resends
		if aborted {
			result = "aborted"
			return
		}
		if !acked {
			result = "gaveup"
			s.Logger.Warnf("tftp: %s: %q: no ACK for OACK, giving up", client, name)
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
				s.Logger.Errorf("tftp: %s: read %q at %d: %v", client, name, off, err)
				s.sendError(pc, client, errNotDefined, "read error")
				return
			}
		}
		pkt := data[:4+want]
		if s.Logger.Enabled(logging.LevelDebug) {
			s.Logger.Debugf("tftp: %s: DATA block %d (%d bytes, off %d)", client, block, want, off)
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

		// The block is put on the wire by sendAndWaitAck, so count it as
		// transmitted before we know whether it was acked.
		blocksSent++
		bytesSent += want
		acked, aborted, resends := s.sendAndWaitAck(pc, client, clientAP, ack, pkt, block, blockTimeout, blockRetries)
		resendTotal += resends
		if acked {
			bytesAcked += want
		}
		switch {
		case aborted:
			result = "aborted"
			return
		case isFinal:
			// An unacked final block is the normal SGI PROM case, not a
			// failure: the client stopped listening once it had the file.
			// Give it its own result so the summary does not warn on it.
			if acked {
				result = "ok"
				s.Logger.Infof("tftp: %s: %q done", client, name)
			} else {
				result = "unacked_final"
				s.Logger.Infof("tftp: %s: %q sent; final block unacked (client done)", client, name)
			}
			return
		case !acked:
			result = "gaveup"
			s.Logger.Warnf("tftp: %s: %q: no ACK for block %d, giving up", client, name, block)
			return
		}
		block++
	}
}
