package status

import (
	"bytes"
	"strings"
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

// stuck is a reader that never drains, as a front end that stopped reading.
type stuck struct{ release chan struct{} }

func (s stuck) Write(p []byte) (int, error) { <-s.release; return len(p), nil }

func TestStuckReaderNeverBlocksProgress(t *testing.T) {
	r := stuck{release: make(chan struct{})}
	w := New(r)
	done := make(chan struct{})
	go func() {
		for range queueLen * 2 {
			w.TFTP("sgi", "/5.3/dist/sa", "progress", 10, 5, "")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("progress lines blocked on a reader that is not reading")
	}
	close(r.release)
}

func TestNothingFollowsStopped(t *testing.T) {
	var b bytes.Buffer
	w := New(&b)
	w.Stopped("signal", "")
	w.Inst("sgi", "session_end", "ok") // a session that outlived shutdown
	if got := strings.Count(b.String(), "\n"); got != 1 || !strings.Contains(b.String(), `"type":"stopped"`) {
		t.Errorf("output %q, want only the stopped line", b.String())
	}
}
