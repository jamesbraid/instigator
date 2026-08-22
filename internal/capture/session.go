package capture

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// markerRe matches inst's completion-marker echo. On the real wire (a
// captured Octane session) almost every line carries it: inst appends the
// wrapper to the command it reports on, all in one line -
//
//	dd if=/6.5.30/dist/foo bs=512 ; ( status=$? ; trap '' 2 ; echo 'o?_InstProc421IsDone\c' ; echo 'o?_InstProc421IsDone'$status'\c' 1>&2 )
//
// with an InstKill variant for its flush/kill probes. Recording the flag
// faithfully keeps events.jsonl truthful about the wire; the summarizer
// uses markerPayload to pull the wrapped command back out.
var markerRe = regexp.MustCompile(`Inst(?:Proc|Kill)[0-9]+IsDone`)

func isMarker(line string) bool { return markerRe.MatchString(line) }

// wrapperRe matches the wrapper block itself: the trailing subshell that
// captures $? and echoes the marker.
var wrapperRe = regexp.MustCompile(`\(\s*status=\$\?.*IsDone.*\)\s*$`)

// markerPayload returns the command a marker line wraps, or "" when the
// line is protocol bookkeeping only - the session-opening "trap : 2" arm,
// or a bare wrapper. In the captured corpus every other marker line wraps
// a real command (dd, ls, echo).
func markerPayload(line string) string {
	loc := wrapperRe.FindStringIndex(line)
	if loc == nil {
		return ""
	}
	p := strings.TrimSpace(line[:loc[0]])
	p = strings.TrimSpace(strings.TrimSuffix(p, ";"))
	if p == "" || firstVerb(p) == "trap" {
		return ""
	}
	return p
}

// Session is one rsh connection's recording context. It is created by
// BeginSession, hands out per-command counting wrappers, and is closed by
// End. A nil *Session is a no-op, so instcmd can hold one unconditionally.
//
// The rsh shell runs one command at a time, but a single command's
// pipeline stages (cat a | fgrep b) execute in concurrent goroutines that
// both write the current command's counters - so the counters are atomic.
// cur is an atomic pointer because the session-level stdout wrapper reads
// it on every write while BeginCommand replaces it between commands.
type Session struct {
	rec      *Recorder
	id       string
	alias    string
	start    time.Time
	cmdCount int
	bytesOut atomic.Int64
	cur      atomic.Pointer[Command]
}

// Command is one wire line's recording context: its counters and the files
// it served, snapshotted into an inst_command_end event by End.
type Command struct {
	sess   *Session
	seq    int
	line   string
	marker bool
	start  time.Time

	efsReadCalls atomic.Int64
	efsReadBytes atomic.Int64
	stdoutCalls  atomic.Int64
	stdoutBytes  atomic.Int64
	stderrCalls  atomic.Int64
	stderrBytes  atomic.Int64

	mu     sync.Mutex
	served []served
}

// BeginSession emits rsh_session_start and returns the session's recording
// context. A nil Recorder yields a nil Session.
func (r *Recorder) BeginSession(alias, remoteAddr, remoteUser, localUser string) *Session {
	if r == nil {
		return nil
	}
	id := fmt.Sprintf("s%d", r.sessionSeq.Add(1))
	s := &Session{rec: r, id: id, alias: alias, start: r.now()}
	e := &rshSessionStart{RemoteAddr: remoteAddr, RemoteUser: remoteUser, LocalUser: localUser}
	e.Event = "rsh_session_start"
	e.Session = id
	e.Client = alias
	r.emit(e)
	return s
}

// RSHRejected records a connection rcmd refused before any session began -
// a non-reserved source port, or a client outside the allow list. It is a
// start/end pair with result "refused" so it shows in the session list as
// a connection that never became a session. reason is a short class.
func (r *Recorder) RSHRejected(alias, remoteAddr, reason string) {
	if r == nil {
		return
	}
	id := fmt.Sprintf("s%d", r.sessionSeq.Add(1))
	start := &rshSessionStart{RemoteAddr: remoteAddr}
	start.Event = "rsh_session_start"
	start.Session = id
	start.Client = alias
	r.emit(start)
	end := &rshSessionEnd{}
	end.Event = "rsh_session_end"
	end.Session = id
	end.Client = alias
	end.Result = "refused"
	end.Err = reason
	r.emit(end)
}

// End emits rsh_session_end, classifying the outcome: a nil error is a
// clean close, a deadline is the idle timeout firing, anything else is an
// interrupted session (a lost connection mid-stream).
func (s *Session) End(err error) {
	if s == nil {
		return
	}
	e := &rshSessionEnd{
		DurationMS:   s.rec.now().Sub(s.start).Milliseconds(),
		CommandCount: s.cmdCount,
		BytesOut:     s.bytesOut.Load(),
	}
	e.Event = "rsh_session_end"
	e.Session = s.id
	e.Client = s.alias
	switch {
	case err == nil:
		e.Result = "ok"
	case errors.Is(err, os.ErrDeadlineExceeded):
		e.Result = "idle"
	default:
		e.Result = "interrupted"
		e.Err = errClass(err)
	}
	s.rec.emit(e)
}

// BeginCommand emits inst_command_start and makes this command current, so
// the session's counting wrappers attribute to it. Every non-empty wire
// line is a command, including the marker wrapper.
func (s *Session) BeginCommand(line string) *Command {
	if s == nil {
		return nil
	}
	s.cmdCount++
	c := &Command{sess: s, seq: s.cmdCount, line: line, marker: isMarker(line), start: s.rec.now()}
	s.cur.Store(c)
	e := &instCommandStart{Seq: c.seq, Line: line, Marker: c.marker}
	e.Event = "inst_command_start"
	e.Session = s.id
	e.Client = s.alias
	s.rec.emit(e)
	return c
}

// End emits inst_command_end with the command's counters and served files.
// refused marks a command this shell would not run (not whitelisted, a
// write attempt); a nonzero exitStatus without refusal is a command that
// ran and failed.
func (c *Command) End(exitStatus int, refused bool) {
	if c == nil {
		return
	}
	s := c.sess
	c.mu.Lock()
	served := append([]served(nil), c.served...)
	c.mu.Unlock()
	e := &instCommandEnd{
		Seq:          c.seq,
		Line:         c.line,
		Verb:         firstVerb(c.line),
		ExitStatus:   exitStatus,
		DurationMS:   s.rec.now().Sub(c.start).Milliseconds(),
		EFSReadCalls: c.efsReadCalls.Load(),
		EFSReadBytes: c.efsReadBytes.Load(),
		StdoutCalls:  c.stdoutCalls.Load(),
		StdoutBytes:  c.stdoutBytes.Load(),
		StderrCalls:  c.stderrCalls.Load(),
		StderrBytes:  c.stderrBytes.Load(),
		Served:       served,
		Marker:       c.marker,
	}
	e.Event = "inst_command_end"
	e.Session = s.id
	e.Client = s.alias
	switch {
	case refused:
		e.Result = "refused"
	case exitStatus != 0:
		e.Result = "nonzero"
	default:
		e.Result = "ok"
	}
	s.rec.emit(e)
}

// RecordServed notes a file a leaf command actually read, with the backing
// image and in-image path when the tree could resolve it. Safe to call
// from concurrent pipeline stages.
func (s *Session) RecordServed(treePath, image, imagePath string) {
	if s == nil {
		return
	}
	c := s.cur.Load()
	if c == nil {
		return
	}
	c.mu.Lock()
	c.served = append(c.served, served{TreePath: treePath, Image: image, ImagePath: imagePath})
	c.mu.Unlock()
}

// WrapWriter wraps a session output stream, both of which are bound to the
// rsh socket, so every write is counted against the session total and the
// current command. stderr selects which per-command counters it feeds, so
// dd's data (stdout) and its records summary (stderr) are told apart.
// Returns w unchanged on a nil session.
func (s *Session) WrapWriter(w io.Writer, stderr bool) io.Writer {
	if s == nil {
		return w
	}
	return &countingWriter{w: w, sess: s, stderr: stderr}
}

// WrapReaderAt wraps a backing EFS file so its ReadAt calls and bytes are
// counted against the command current when the file was opened - which is
// the command that reads it, since these leaf commands open and consume a
// file within one line. Returns r unchanged on a nil session.
func (s *Session) WrapReaderAt(r io.ReaderAt) io.ReaderAt {
	if s == nil {
		return r
	}
	return &countingReaderAt{r: r, cmd: s.cur.Load()}
}

type countingWriter struct {
	w      io.Writer
	sess   *Session
	stderr bool
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.sess.bytesOut.Add(int64(n))
	if cmd := c.sess.cur.Load(); cmd != nil {
		if c.stderr {
			cmd.stderrCalls.Add(1)
			cmd.stderrBytes.Add(int64(n))
		} else {
			cmd.stdoutCalls.Add(1)
			cmd.stdoutBytes.Add(int64(n))
		}
	}
	return n, err
}

type countingReaderAt struct {
	r   io.ReaderAt
	cmd *Command
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	if c.cmd != nil {
		c.cmd.efsReadCalls.Add(1)
		c.cmd.efsReadBytes.Add(int64(n))
	}
	return n, err
}

// firstVerb is the command's verb: the base name of its first word, so
// /usr/bin/dd and dd summarize together. "" for a blank line.
func firstVerb(line string) string {
	f := strings.Fields(line)
	if len(f) == 0 {
		return ""
	}
	v := f[0]
	if i := strings.LastIndex(v, "/"); i >= 0 {
		v = v[i+1:]
	}
	return v
}

// errClass reduces an error to a short, non-identifying class for the
// event's err field, never the raw text.
func errClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "deadline"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof"
	case errors.Is(err, os.ErrClosed):
		return "closed"
	default:
		return "error"
	}
}
