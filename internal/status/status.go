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

// Writer serializes lines from concurrent transfers and sessions.
type Writer struct {
	mu  sync.Mutex
	enc *json.Encoder
	now func() time.Time
}

// New writes lines to w.
func New(w io.Writer) *Writer {
	return &Writer{enc: json.NewEncoder(w), now: time.Now}
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
	w.write("tftp", func(h header) any {
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
func (w *Writer) Stopped(reason, err string) {
	w.write("stopped", func(h header) any {
		return struct {
			header
			Reason string `json:"reason"`
			Error  string `json:"error,omitempty"`
		}{h, reason, err}
	})
}

// write stamps the header under the lock, so lines appear in time order.
func (w *Writer) write(typ string, line func(header) any) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.enc.Encode(line(header{version, w.now().UTC().Format(time.RFC3339Nano), typ}))
}
