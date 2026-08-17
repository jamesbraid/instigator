package capture

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
	if r == nil {
		return
	}
	e := &bootpReply{MAC: mac, File: file, OfferedIP: offeredIP}
	e.Event = "bootp_reply"
	e.Client = alias
	e.Result = result
	r.emit(e)
}
