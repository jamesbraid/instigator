package status

import (
	"bytes"
	"testing"
	"time"
)

func TestLinesCarryHeaderThenFields(t *testing.T) {
	var b bytes.Buffer
	w := New(&b)
	w.now = func() time.Time { return time.Date(2026, 11, 23, 1, 2, 3, 0, time.UTC) }
	w.Ready("192.0.2.10", "socket", []Set{{Name: "5.3", Dist: "/5.3/dist"}})
	w.TFTP("sgi", "/5.3/dist/sa", "end", 2048, 1024, "aborted")
	w.Stopped("guest_disconnected", "")

	want := `{"v":1,"time":"2026-11-23T01:02:03Z","type":"ready","server_ip":"192.0.2.10","network":"socket","sets":[{"name":"5.3","dist":"/5.3/dist"}]}
{"v":1,"time":"2026-11-23T01:02:03Z","type":"tftp","client":"sgi","file":"/5.3/dist/sa","phase":"end","size":2048,"bytes_sent":1024,"result":"aborted"}
{"v":1,"time":"2026-11-23T01:02:03Z","type":"stopped","reason":"guest_disconnected"}
`
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
}

func TestNilWriterIsNoOp(t *testing.T) {
	var w *Writer
	w.Inst("sgi", "go_returned", "")
}
