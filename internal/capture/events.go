package capture

// header is the envelope every event carries, embedded in each event
// struct so its fields land at the top level of the JSON object. The
// recorder fills V/TS/Run at emit time; each event method fills Event
// and, where they apply, Session/Client/Result/Err.
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

func (h *header) setEnvelope(v int, ts, run string) {
	h.V = v
	h.TS = ts
	h.Run = run
}

// envelopeSetter is what emit needs from an event; every event satisfies
// it through the embedded header.
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
	e := &serverEvent{}
	e.Event = "server_start"
	r.emit(e)
}

// ServerStop records a clean shutdown. reason is the stop cause (today
// always "clean"); it rides in Result so the summary can tell a graceful
// stop from a future abnormal one.
func (r *Recorder) ServerStop(reason string) {
	e := &serverEvent{}
	e.Event = "server_stop"
	e.Result = reason
	r.emit(e)
}

type listenerExit struct {
	header
	Listener string `json:"listener"`
}

// ListenerExit records a protocol listener returning while the server was
// not shutting down, so a listener that died mid-run is visible in both
// the log and the trace.
func (r *Recorder) ListenerExit(name string) {
	e := &listenerExit{Listener: name}
	e.Event = "listener_exit"
	e.Result = "error"
	r.emit(e)
}

type bootpReply struct {
	header
	MAC       string `json:"mac,omitempty"`
	File      string `json:"requested_file,omitempty"`
	OfferedIP string `json:"offered_ip,omitempty"`
}

// BootpReply records the outcome of a BOOTP request: answered (a
// configured client was sent its reply), or ignored (a well-formed request
// from a MAC that is not configured). alias is the client's configured
// name, empty when unknown.
func (r *Recorder) BootpReply(alias, mac, file, offeredIP, result string) {
	e := &bootpReply{MAC: mac, File: file, OfferedIP: offeredIP}
	e.Event = "bootp_reply"
	e.Client = alias
	e.Result = result
	r.emit(e)
}

// TransferRecord is one completed TFTP transfer, assembled by the tftp
// server and handed here for recording. Result is the outcome: ok,
// unacked_final (all blocks sent but the client never acked the last one,
// the normal SGI PROM case), aborted (the client sent ERROR or a send
// failed), gaveup (a mid-transfer block went unacked within the retry
// budget), notfound, or error. BytesSent is what went on the wire;
// BytesAcked is what the client confirmed - they differ exactly when the
// final block is unacked.
type TransferRecord struct {
	Client      string
	Name        string
	TreePath    string
	Image       string
	ImagePath   string
	Size        int64
	BlockSize   int
	Blocks      int
	BytesSent   int64
	BytesAcked  int64
	Retransmits int
	DurationMS  int64
	Result      string
}

type tftpTransferEnd struct {
	header
	Name        string `json:"requested_name"`
	TreePath    string `json:"tree_path,omitempty"`
	Image       string `json:"image,omitempty"`
	ImagePath   string `json:"image_path,omitempty"`
	Size        int64  `json:"size"`
	BlockSize   int    `json:"block_size"`
	Blocks      int    `json:"blocks"`
	BytesSent   int64  `json:"bytes_sent"`
	BytesAcked  int64  `json:"bytes_acked"`
	Retransmits int    `json:"retransmits"`
	DurationMS  int64  `json:"duration_ms"`
}

// TFTPTransferEnd records a completed (or abandoned) TFTP transfer. There
// is no start event: a boot transfer is short, and a missing end for an
// interrupted one is not worth a second line here.
func (r *Recorder) TFTPTransferEnd(rec TransferRecord) {
	e := &tftpTransferEnd{
		Name:        rec.Name,
		TreePath:    rec.TreePath,
		Image:       rec.Image,
		ImagePath:   rec.ImagePath,
		Size:        rec.Size,
		BlockSize:   rec.BlockSize,
		Blocks:      rec.Blocks,
		BytesSent:   rec.BytesSent,
		BytesAcked:  rec.BytesAcked,
		Retransmits: rec.Retransmits,
		DurationMS:  rec.DurationMS,
	}
	e.Event = "tftp_transfer_end"
	e.Client = rec.Client
	e.Result = rec.Result
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
