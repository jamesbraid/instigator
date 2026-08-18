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
		prov.End = r.now().UTC().Format(time.RFC3339Nano)
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
