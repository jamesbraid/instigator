package capture

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
	if r == nil {
		return
	}
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
