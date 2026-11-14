package qemunet

import (
	"sync"
	"time"

	"github.com/jamesbraid/instigator/internal/logging"
)

// writeWatch logs when a write to the stream blocks past a threshold, and
// when it finishes. One timer is reused for every write, so the cost per
// write is a Reset and a Stop, not an allocation.
type writeWatch struct {
	log       *logging.Logger
	threshold time.Duration
	timer     *time.Timer

	mu      sync.Mutex
	armed   bool
	started time.Time
	warned  bool
}

func newWriteWatch(log *logging.Logger, threshold time.Duration) *writeWatch {
	w := &writeWatch{log: log, threshold: threshold}
	w.timer = time.AfterFunc(time.Hour, w.fire)
	w.timer.Stop()
	return w
}

// arm marks the start of a write.
func (w *writeWatch) arm() {
	w.mu.Lock()
	w.armed, w.warned, w.started = true, false, time.Now()
	w.mu.Unlock()
	w.timer.Reset(w.threshold)
}

func (w *writeWatch) fire() {
	w.mu.Lock()
	defer w.mu.Unlock()
	// A fire that was already running when the write finished and the next
	// one began must not blame the new write.
	if !w.armed || time.Since(w.started) < w.threshold {
		return
	}
	w.warned = true
	w.log.Warnf("qemunet: write to the machine has been blocked for %s; the machine is not reading the network stream", w.threshold)
}

// disarm marks the end of a write that returned err.
func (w *writeWatch) disarm(err error) {
	w.timer.Stop()
	w.mu.Lock()
	warned, took := w.warned, time.Since(w.started)
	w.armed, w.warned = false, false
	w.mu.Unlock()
	if !warned {
		return
	}
	if err != nil {
		w.log.Warnf("qemunet: write to the machine failed after %s blocked: %v", took.Round(time.Millisecond), err)
		return
	}
	w.log.Infof("qemunet: write to the machine recovered after %s", took.Round(time.Millisecond))
}

func (w *writeWatch) close() { w.timer.Stop() }
