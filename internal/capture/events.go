package capture

// header is the envelope every event carries. It is embedded (anonymous)
// in each event struct, so encoding/json promotes these fields to the top
// level of the JSON object alongside the event's own payload. The
// recorder fills V/TS/Run via setEnvelope at emit time; each event method
// fills Event and, where they apply, Session/Client/Result/Err before
// emitting.
type header struct {
	V       int    `json:"v"`
	TS      string `json:"ts"`
	Run     string `json:"run"`
	Event   string `json:"event"`
	Session string `json:"session,omitempty"`
	Client  string `json:"client,omitempty"`
	Result  string `json:"result,omitempty"`
	Err     string `json:"err,omitempty"`
}

// setEnvelope stamps the run-wide fields. Promoted onto every event
// through the embedded header, it lets emit fill the envelope without
// knowing the concrete event type.
func (h *header) setEnvelope(v int, ts, run string) {
	h.V = v
	h.TS = ts
	h.Run = run
}

// envelopeSetter is what emit needs from an event: the ability to receive
// the run-wide envelope. Every event satisfies it through the embedded
// *header method set.
type envelopeSetter interface {
	setEnvelope(v int, ts, run string)
}

// serverEvent carries no payload beyond the envelope; enabled services
// and provenance live in run.json, not here.
type serverEvent struct {
	header
}

// ServerStart records that the server bound its listeners and began
// serving.
func (r *Recorder) ServerStart() {
	if r == nil {
		return
	}
	e := &serverEvent{}
	e.Event = "server_start"
	r.emit(e)
}

// ServerStop records a clean shutdown. reason is the stop cause (today
// always "clean"); it rides in Result so the summary can tell a graceful
// stop from a future abnormal one.
func (r *Recorder) ServerStop(reason string) {
	if r == nil {
		return
	}
	e := &serverEvent{}
	e.Event = "server_stop"
	e.Result = reason
	r.emit(e)
}

// served is one file a command read, with the backing image and in-image
// path when the tree could resolve them.
type served struct {
	TreePath  string `json:"tree_path"`
	Image     string `json:"image,omitempty"`
	ImagePath string `json:"image_path,omitempty"`
}

type rshSessionStart struct {
	header
	RemoteAddr string `json:"remote_addr,omitempty"`
	RemoteUser string `json:"remote_user,omitempty"`
	LocalUser  string `json:"local_user,omitempty"`
}

type rshSessionEnd struct {
	header
	DurationMS   int64 `json:"duration_ms"`
	CommandCount int   `json:"command_count"`
	BytesOut     int64 `json:"bytes_out"`
}

type instCommandStart struct {
	header
	Seq    int    `json:"seq"`
	Line   string `json:"line"`
	Marker bool   `json:"marker,omitempty"`
}

type instCommandEnd struct {
	header
	Seq          int      `json:"seq"`
	Line         string   `json:"line"`
	Verb         string   `json:"verb"`
	ExitStatus   int      `json:"exit_status"`
	DurationMS   int64    `json:"duration_ms"`
	EFSReadCalls int64    `json:"efs_read_calls"`
	EFSReadBytes int64    `json:"efs_read_bytes"`
	StdoutCalls  int64    `json:"stdout_calls"`
	StdoutBytes  int64    `json:"stdout_bytes"`
	StderrCalls  int64    `json:"stderr_calls"`
	StderrBytes  int64    `json:"stderr_bytes"`
	Served       []served `json:"served,omitempty"`
	Marker       bool     `json:"marker,omitempty"`
}
