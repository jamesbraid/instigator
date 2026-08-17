package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type listenerExit struct {
	header
	Listener   string `json:"listener"`
	Unexpected bool   `json:"unexpected"`
}

// ListenerExit records a protocol listener returning while the server was
// not shutting down - an unexpected exit that the shutdown summary flags,
// so a listener that died mid-run is visible in both the log and the trace.
func (r *Recorder) ListenerExit(name string, unexpected bool, errClass string) {
	if r == nil {
		return
	}
	e := &listenerExit{Listener: name, Unexpected: unexpected}
	e.Event = "listener_exit"
	e.Result = "error"
	e.Err = errClass
	r.emit(e)
}

// Finish closes the run at a clean shutdown: it emits server_stop, flushes
// and closes events.jsonl, rewrites run.json with the end time, then reads
// the events back to write summary.json and render the human summary. It
// must run after every session and transfer has ended; a late event from an
// in-flight goroutine is safely dropped once events.jsonl is closed.
func (r *Recorder) Finish(reason string) error {
	if r == nil {
		return nil
	}
	r.ServerStop(reason)

	r.mu.Lock()
	var closeErr error
	if r.events != nil {
		closeErr = r.events.Close()
		r.events = nil
	}
	prov := r.prov
	writeErr := r.writeErr
	r.mu.Unlock()
	if writeErr == nil {
		writeErr = closeErr // a failed flush-on-close also means events.jsonl is suspect
	}

	// run.json gets its end time only if provenance was written at all.
	if prov.Schema != 0 {
		prov.End = r.now().UTC().Format(time.RFC3339Nano)
		if err := r.writeRunLocked(prov); err != nil {
			return err
		}
	}

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

	if r.summaryW != nil {
		sum.WriteText(r.summaryW)
	}
	// An event dropped mid-run means the summary is computed from an
	// incomplete file; report it so the operator does not trust it blindly.
	return writeErr
}
