// Package status writes serve --json: one JSON object per line on stdout,
// in the schema docs/json-output.md documents. A nil *Writer is a no-op.
package status

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// version is the "v" every line carries. Bump it only for an incompatible
// change; new types and fields do not need one.
const version = 1

// queueLen bounds the lines waiting for a slow reader. Past it, progress
// lines are dropped and other lines wait.
const queueLen = 1024

// Writer serializes lines from concurrent transfers and sessions. A
// goroutine writes them, so serving never waits on the reader.
type Writer struct {
	mu     sync.Mutex
	now    func() time.Time
	lines  chan []byte
	done   chan struct{}
	closed bool
}

// New writes lines to w until Stopped.
func New(w io.Writer) *Writer {
	st := &Writer{now: time.Now, lines: make(chan []byte, queueLen), done: make(chan struct{})}
	go func() {
		defer close(st.done)
		for b := range st.lines {
			w.Write(b) // a reader that went away loses its lines, not the install
		}
	}()
	return st
}

// header leads every line; each type embeds it so its fields follow.
type header struct {
	V    int    `json:"v"`
	Time string `json:"time"`
	Type string `json:"type"`
}

// Set is one served install set.
type Set struct {
	Name string `json:"name"`
	Dist string `json:"dist"`
}

// Ready reports that the media are assembled and every listener is up.
// network is host, socket or tcp.
func (w *Writer) Ready(serverIP, network string, sets []Set) {
	w.write("ready", func(h header) any {
		return struct {
			header
			ServerIP string `json:"server_ip"`
			Network  string `json:"network"`
			Sets     []Set  `json:"sets"`
		}{h, serverIP, network, sets}
	})
}

// BOOTP reports one BOOTP or DHCP request and what was done with it.
func (w *Writer) BOOTP(client, mac, ip, file, result string) {
	w.write("bootp", func(h header) any {
		return struct {
			header
			Client string `json:"client,omitempty"`
			MAC    string `json:"mac"`
			IP     string `json:"ip,omitempty"`
			File   string `json:"file,omitempty"`
			Result string `json:"result"`
		}{h, client, mac, ip, file, result}
	})
}

// TFTP reports a transfer's start, its progress, or its end. Only an end
// carries a result.
func (w *Writer) TFTP(client, file, phase string, size, bytesSent int64, result string) {
	w.writeLine("tftp", phase == "progress", func(h header) any {
		return struct {
			header
			Client    string `json:"client"`
			File      string `json:"file"`
			Phase     string `json:"phase"`
			Size      int64  `json:"size"`
			BytesSent int64  `json:"bytes_sent"`
			Result    string `json:"result,omitempty"`
		}{h, client, file, phase, size, bytesSent, result}
	})
}

// Inst reports an rsh session from inst starting or ending, or inst's go
// returning. A return does not mean the install succeeded.
func (w *Writer) Inst(client, phase, result string) {
	w.write("inst", func(h header) any {
		return struct {
			header
			Client string `json:"client"`
			Phase  string `json:"phase"`
			Result string `json:"result,omitempty"`
		}{h, client, phase, result}
	})
}

// Stopped is the last line: reason is signal, guest_disconnected or error.
// It waits for every queued line to be written and drops any later ones.
func (w *Writer) Stopped(reason, err string) {
	w.write("stopped", func(h header) any {
		return struct {
			header
			Reason string `json:"reason"`
			Error  string `json:"error,omitempty"`
		}{h, reason, err}
	})
	if w == nil {
		return
	}
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.lines)
	}
	w.mu.Unlock()
	<-w.done
}

func (w *Writer) write(typ string, line func(header) any) { w.writeLine(typ, false, line) }

// writeLine stamps and queues under the lock, so lines appear in time order.
// A droppable line is skipped when the queue is full.
func (w *Writer) writeLine(typ string, droppable bool, line func(header) any) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	b, err := json.Marshal(line(header{version, w.now().UTC().Format(time.RFC3339Nano), typ}))
	if err != nil {
		return
	}
	b = append(b, '\n')
	if droppable {
		select {
		case w.lines <- b:
		default:
		}
		return
	}
	w.lines <- b
}
