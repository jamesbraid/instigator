package capture

import "testing"

func TestTFTPTransferEndEvent(t *testing.T) {
	r, dir := newTestRecorder(t)
	r.TFTPTransferEnd(TransferRecord{
		Client:      "192.0.2.10",
		Name:        "/fx.64",
		TreePath:    "stand/fx.64",
		Image:       "tools.iso",
		ImagePath:   "stand/fx.64",
		Size:        4096,
		BlockSize:   512,
		Blocks:      8,
		BytesSent:   4096,
		Retransmits: 2,
		DurationMS:  12,
		Result:      "ok",
	})
	r.Close()

	e := only(t, dir, "tftp_transfer_end")
	if e["client"] != "192.0.2.10" {
		t.Errorf("client = %v", e["client"])
	}
	if e["requested_name"] != "/fx.64" {
		t.Errorf("requested_name = %v", e["requested_name"])
	}
	if e["image"] != "tools.iso" {
		t.Errorf("image = %v", e["image"])
	}
	if e["bytes_sent"] != float64(4096) {
		t.Errorf("bytes_sent = %v", e["bytes_sent"])
	}
	if e["blocks"] != float64(8) {
		t.Errorf("blocks = %v", e["blocks"])
	}
	if e["retransmits"] != float64(2) {
		t.Errorf("retransmits = %v", e["retransmits"])
	}
	if e["result"] != "ok" {
		t.Errorf("result = %v", e["result"])
	}
}
