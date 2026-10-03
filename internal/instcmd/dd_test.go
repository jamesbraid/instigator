package instcmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/jamesbraid/instigator/internal/logging"
	"mvdan.cc/sh/v3/interp"
)

func TestShellDDBatchesOutput(t *testing.T) {
	payload := make([]byte, 1024*1024+17)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	fsys := &fakeFS{files: map[string][]byte{"data": payload}}
	var out bytes.Buffer
	var calls int
	w := writerFunc(func(p []byte) (int, error) {
		calls++
		return out.Write(p)
	})
	logger := logging.New(io.Discard, logging.LevelInfo)
	if err := RunShell(fsys, strings.NewReader("dd if=/data bs=512\necho 'done\\c'\n"), w, io.Discard, logger, nil); err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), payload...), "done"...)
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatal("payload or trailing marker differs")
	}
	if calls > 258 {
		t.Fatalf("%d stdout writes for 1 MiB: small input blocks were not batched", calls)
	}
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

func TestDDRejectsShortOutput(t *testing.T) {
	hc := interp.HandlerContext{
		Stdin: strings.NewReader("input"),
		Stdout: writerFunc(func(p []byte) (int, error) {
			return len(p) - 1, nil
		}),
		Stderr: io.Discard,
	}
	err := shDD(ddTestEnv(t, shellTestFS()), hc, nil)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short output returned %v, want %v", err, io.ErrShortWrite)
	}
}

func TestDDReturnsOutputError(t *testing.T) {
	hc := interp.HandlerContext{
		Stdin:  strings.NewReader("input"),
		Stdout: writerFunc(func([]byte) (int, error) { return 0, io.ErrClosedPipe }),
		Stderr: io.Discard,
	}
	err := shDD(ddTestEnv(t, shellTestFS()), hc, nil)
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("output error = %v, want %v", err, io.ErrClosedPipe)
	}
}

func TestDDRejectsZeroProgress(t *testing.T) {
	for _, block := range []int{512, 65536} {
		t.Run(fmt.Sprint(block), func(t *testing.T) {
			calls := 0
			hc := interp.HandlerContext{
				Stdin: bytes.NewReader(make([]byte, block)),
				Stdout: writerFunc(func([]byte) (int, error) {
					calls++
					if calls > 1 {
						// Bound a retry loop so a regression fails instead of hanging.
						return 0, io.ErrClosedPipe
					}
					return 0, nil
				}),
				Stderr: io.Discard,
			}
			err := shDD(ddTestEnv(t, shellTestFS()), hc,
				[]string{fmt.Sprintf("bs=%d", block)})
			if !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("zero-progress output returned %v, want %v", err, io.ErrShortWrite)
			}
		})
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func TestDDFlushesBeforeReadError(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 600)
	readErr := errors.New("backing read failed")
	var out bytes.Buffer
	hc := interp.HandlerContext{
		Stdin:  io.MultiReader(bytes.NewReader(payload), errorReader{readErr}),
		Stdout: &out,
		Stderr: writerFunc(func(p []byte) (int, error) {
			if !bytes.Equal(out.Bytes(), payload) {
				t.Fatal("read diagnostic preceded pending stdout")
			}
			return len(p), nil
		}),
	}
	err := shDD(ddTestEnv(t, shellTestFS()), hc, nil)
	if err == nil {
		t.Fatal("read failure was lost")
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Fatal("partial input before read error was lost")
	}
}

func TestDDRecordCountsAfterBatching(t *testing.T) {
	for _, tc := range []struct {
		name, operands, records string
		start, end              int
	}{
		{"tail", "bs=512", "3+1", 0, 2000},
		{"count", "bs=512 count=2", "2+0", 0, 1024},
		{"skip", "bs=512 skip=2 count=1", "1+0", 1024, 1536},
		{"zero", "bs=512 count=0", "0+0", 0, 0},
		{"unaligned", "bs=7", "285+1", 0, 2000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := shellTestFS()
			var out, stderr bytes.Buffer
			hc := interp.HandlerContext{Stdout: &out, Stderr: &stderr}
			err := shDD(ddTestEnv(t, fsys), hc,
				append([]string{"if=/6.5.30/disc1/dist/sa"}, strings.Fields(tc.operands)...))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Bytes(), fsys.files["6.5.30/disc1/dist/sa"][tc.start:tc.end]) {
				t.Fatal("copied range differs")
			}
			want := tc.records + " records in\n" + tc.records + " records out\n"
			if stderr.String() != want {
				t.Fatalf("records = %q, want %q", stderr.String(), want)
			}
		})
	}
}

func TestDDRecordCountsAcrossGroups(t *testing.T) {
	for _, tc := range []struct {
		block   string
		records string
	}{
		{"512", "136+1"},
		{"7", "10002+1"},
		{"1000", "70+1"},
		{"65536", "1+1"},
	} {
		t.Run(tc.block, func(t *testing.T) {
			payload := make([]byte, 70017)
			for i := range payload {
				payload[i] = byte(i % 251)
			}
			var out, stderr bytes.Buffer
			hc := interp.HandlerContext{Stdin: bytes.NewReader(payload), Stdout: &out, Stderr: &stderr}
			err := shDD(ddTestEnv(t, shellTestFS()), hc,
				[]string{"bs=" + tc.block})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Bytes(), payload) {
				t.Fatal("grouped data differs")
			}
			want := tc.records + " records in\n" + tc.records + " records out\n"
			if stderr.String() != want {
				t.Fatalf("records = %q, want %q", stderr.String(), want)
			}
		})
	}
}

func ddTestEnv(t *testing.T, fsys FileSystem) *shellEnv {
	t.Helper()
	env := newShellEnv(fsys, logging.New(io.Discard, logging.LevelInfo))
	env.transfers = newTransferInterruptController(nil, nil)
	t.Cleanup(env.transfers.close)
	return env
}
