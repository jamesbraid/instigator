package rcmd_test

import (
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/rcmd"
)

// TestConcurrentStderrDialbackDoesNotDegrade is a regression guard for
// the lead investigated in the "Lost connection" report: inst opens a
// burst of shell sessions together to read more than one distribution's
// product descriptions, each requesting a stderr dial-back, and
// dialStderr's reserved-port scan (1023 down to 512) was a suspect for
// dropped or delayed connections under that burst.
//
// It doesn't reproduce a drop. Measured against this package directly
// (which can bind <1024 unprivileged in this environment, exercising
// the real scan): at n=5, matching the actual burst size a serve.log
// capture showed (several "exec /bin/sh" connections inside the same
// wall-clock second), every dial-back resolved in under a millisecond.
// Stressed here to n=20 - four times that - it still must finish
// quickly and never fail; separate manual testing pushed this to
// n=100 (25x the observed burst) and still saw zero failures, worst
// case under 100ms. None of that explains a multi-second "Lost
// connection"; this test exists to catch a real regression in the scan
// (an actual hang or failure under burst), not to explain the report.
func TestConcurrentStderrDialbackDoesNotDegrade(t *testing.T) {
	const n = 20
	addr := startServer(t, &rcmd.Server{
		AllowHighPorts: true,
		Handler: func(req *rcmd.Request) error {
			fmt.Fprint(req.Stderr, "ok")
			return nil
		},
	})

	type result struct {
		i   int
		lat time.Duration
		err error
	}
	results := make(chan result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			el, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				results <- result{i, 0, fmt.Errorf("listen: %w", err)}
				return
			}
			defer el.Close()
			errPort := el.Addr().(*net.TCPAddr).Port

			accepted := make(chan time.Time, 1)
			go func() {
				el.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
				ec, err := el.Accept()
				if err != nil {
					return
				}
				defer ec.Close()
				accepted <- time.Now()
				io.Copy(io.Discard, ec)
			}()

			t0 := time.Now()
			c, err := net.Dial("tcp", addr.String())
			if err != nil {
				results <- result{i, 0, fmt.Errorf("dial server: %w", err)}
				return
			}
			defer c.Close()
			fmt.Fprintf(c, "%d\x00u\x00u\x00cmd%d\x00", errPort, i)
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			ack := make([]byte, 1)
			if _, err := io.ReadFull(c, ack); err != nil {
				results <- result{i, 0, fmt.Errorf("ack: %w", err)}
				return
			}
			select {
			case at := <-accepted:
				results <- result{i, at.Sub(t0), nil}
			case <-time.After(5 * time.Second):
				results <- result{i, 0, fmt.Errorf("stderr dial-back never arrived")}
			}
		}(i)
	}
	wg.Wait()
	close(results)

	var maxLat time.Duration
	var failures int
	for r := range results {
		if r.err != nil {
			failures++
			t.Errorf("conn %d: %v", r.i, r.err)
			continue
		}
		t.Logf("conn %d: stderr dial-back latency %v", r.i, r.lat)
		if r.lat > maxLat {
			maxLat = r.lat
		}
	}
	if failures > 0 {
		t.Fatalf("%d of %d concurrent stderr dial-backs failed", failures, n)
	}
	// Generous on purpose: this guards against a real regression (a
	// hang, a serialized scan taking seconds), not the low-millisecond
	// numbers actually measured.
	if maxLat > 2*time.Second {
		t.Fatalf("slowest of %d concurrent stderr dial-backs took %v, want well under 2s", n, maxLat)
	}
}
