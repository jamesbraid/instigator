// Package capture is instigator's opt-in install recorder. Enabled with
// serve --capture-dir, it writes a per-run bundle - run.json (provenance
// and media manifest), events.jsonl (the source of truth: one JSON object
// per BOOTP/TFTP/rsh/inst lifecycle event), and a derived summary.json -
// that becomes debugging data, performance numbers, and the command
// corpus for later CI replay.
//
// The package is a standard-library-only leaf: it never imports config,
// vfs, or any protocol package, so bootp/tftp/instcmd/serve can all
// import it without a cycle. Provenance and event fields arrive as plain
// primitives assembled by the caller.
//
// A *Recorder is nil-safe: every method is a no-op on a nil receiver,
// exactly like logging.Logger. That is the whole "disabled" path - when
// --capture-dir is unset the recorder is nil, no files are opened, and
// callers install none of the counting wrappers, so capture costs
// nothing.
package capture

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// schemaVersion is the trace schema version stamped into every event and
// into run.json. Bump it when the event shape changes incompatibly.
const schemaVersion = 1

// Recorder writes a run's events to events.jsonl and owns the run
// identity and provenance. It is safe for concurrent use: TFTP serves
// each transfer in its own goroutine and multiple rsh sessions run at
// once, so every event write is serialized by mu. A nil *Recorder is a
// no-op.
type Recorder struct {
	dir      string
	runID    string
	summaryW io.Writer // where Finish renders the human summary; default os.Stdout

	sessionSeq atomic.Int64 // hands out session ids s1, s2, ...

	mu       sync.Mutex
	events   *os.File
	prov     Provenance // retained so a clean shutdown can rewrite run.json with the end time
	writeErr error      // first event-write failure, surfaced at Close so a truncated capture is not silent
}

// Option configures a Recorder at construction.
type Option func(*Recorder)

// WithSummaryWriter directs the human summary Finish renders at shutdown to
// w instead of os.Stdout, so a test can capture it.
func WithSummaryWriter(w io.Writer) Option {
	return func(r *Recorder) { r.summaryW = w }
}

// New creates the capture directory (0700) and opens events.jsonl (0600)
// for appending. The run id is generated once here and is stable for the
// life of the Recorder.
func New(dir string, opts ...Option) (*Recorder, error) {
	r := &Recorder{dir: dir, summaryW: os.Stdout}
	for _, o := range opts {
		o(r)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// MkdirAll leaves an existing dir's mode untouched; force it private.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	r.runID = newRunID(time.Now())
	// O_EXCL: a run must have its own fresh directory. Appending to an
	// existing events.jsonl would mix two run ids into one file and quietly
	// corrupt the corpus; fail loudly instead.
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("capture directory %s already holds a run (events.jsonl exists); use a fresh directory", dir)
		}
		return nil, err
	}
	r.events = f
	return r, nil
}

// Close flushes and closes events.jsonl. It is safe on a nil Recorder and
// idempotent enough for a deferred call.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var closeErr error
	if r.events != nil {
		closeErr = r.events.Close()
		r.events = nil
	}
	// A dropped event is worse than a failed close: surface the write
	// error first so an incomplete capture never looks clean.
	if r.writeErr != nil {
		return r.writeErr
	}
	return closeErr
}

// Finish closes the run at a clean shutdown: it emits server_stop, closes
// events.jsonl, rewrites run.json with the end time, then reads the events
// back to write summary.json and render the human summary. It must run
// after every session and transfer has ended; a late event from an
// in-flight goroutine is safely dropped once events.jsonl is closed.
//
// An event dropped mid-run means the summary was computed from an
// incomplete file, so that error outranks anything that goes wrong here.
func (r *Recorder) Finish(reason string) error {
	if r == nil {
		return nil
	}
	r.ServerStop(reason)
	err := r.Close()

	r.mu.Lock()
	prov := r.prov
	r.mu.Unlock()
	// run.json gets its end time only if provenance was written at all.
	if prov.Schema != 0 {
		prov.End = time.Now().UTC().Format(time.RFC3339Nano)
		if werr := r.writeRun(prov); werr != nil && err == nil {
			err = werr
		}
	}
	if serr := r.writeSummary(); serr != nil && err == nil {
		err = serr
	}
	return err
}

// writeSummary reads the closed events file back, writes summary.json, and
// renders the human report.
func (r *Recorder) writeSummary() error {
	f, err := os.Open(filepath.Join(r.dir, "events.jsonl"))
	if err != nil {
		return err
	}
	defer f.Close()
	sum, err := Summarize(f)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.WriteFile(filepath.Join(r.dir, "summary.json"), b, 0o600); err != nil {
		return err
	}
	sum.WriteText(r.summaryW)
	return nil
}

// emit stamps the envelope and appends the event as one JSON line. The
// marshal happens outside the lock (ev is caller-local); only the write is
// serialized, so a partial line can never interleave with another. Its nil
// guard is the one that makes every pure-emit event method nil-safe.
func (r *Recorder) emit(ev envelopeSetter) {
	if r == nil {
		return
	}
	ev.setEnvelope(schemaVersion, time.Now().UTC().Format(time.RFC3339Nano), r.runID)
	b, err := json.Marshal(ev)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		// A dropped event must not be silent: sticky, surfaced at Close.
		if r.writeErr == nil {
			r.writeErr = err
		}
		return
	}
	if r.events == nil {
		return
	}
	if _, err := r.events.Write(append(b, '\n')); err != nil && r.writeErr == nil {
		r.writeErr = err
	}
}

// newRunID is a sortable timestamp plus a short random suffix, unique
// enough to name one run's bundle without coordination.
func newRunID(t time.Time) string {
	var b [4]byte
	rand.Read(b[:])
	return t.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}
