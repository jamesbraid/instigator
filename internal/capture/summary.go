package capture

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"
)

// Summary is the derived view of one run's events.jsonl: per-session
// timing, run-wide totals, per-verb latency, and path reuse. It is what
// summary.json holds and what the shutdown/`trace summary` human report is
// rendered from. Idle is reported per session and never summed run-wide -
// two sessions idle in the same wall-second are not two seconds of idle.
type Summary struct {
	Sessions []SessionSummary `json:"sessions"`

	BytesServed      int64 `json:"bytes_served"`
	Commands         int   `json:"commands"`
	RefusedCommands  int   `json:"refused_commands"`
	NonzeroCommands  int   `json:"nonzero_commands"`
	Transfers        int   `json:"transfers"`
	TFTPRetransmits  int   `json:"tftp_retransmits"`
	AbortedTransfers int   `json:"aborted_transfers"`
	GaveupTransfers  int   `json:"gaveup_transfers"`
	ListenerExits    int   `json:"listener_exits"`
	BootpAnswered    int   `json:"bootp_answered"`

	EFSReadCalls int64 `json:"efs_read_calls"`
	EFSReadBytes int64 `json:"efs_read_bytes"`
	StdoutCalls  int64 `json:"stdout_calls"`
	StdoutBytes  int64 `json:"stdout_bytes"`
	StderrCalls  int64 `json:"stderr_calls"`
	StderrBytes  int64 `json:"stderr_bytes"`

	Verbs   []VerbLatency `json:"verbs"`
	Paths   []PathReuse   `json:"path_reuse"`
	Slowest []SlowCommand `json:"slowest_commands"`
}

// SessionSummary is one rsh session's timing. Active is the sum of its
// command durations, idle is the rest of its wall time - the client
// thinking and the round trips, not server work.
type SessionSummary struct {
	ID        string `json:"id"`
	Client    string `json:"client,omitempty"`
	Result    string `json:"result"`
	WallMS    int64  `json:"wall_ms"`
	ActiveMS  int64  `json:"active_ms"`
	IdleMS    int64  `json:"idle_ms"`
	Commands  int    `json:"commands"`
	BytesOut  int64  `json:"bytes_out"`
	ActiveBps int64  `json:"active_bps"` // bytes_out over active command time: the server-path rate
	WallBps   int64  `json:"wall_bps"`   // bytes_out over wall time: end-to-end, including client think time
}

// VerbLatency is one command verb's duration distribution.
type VerbLatency struct {
	Verb     string `json:"verb"`
	Count    int    `json:"count"`
	MedianMS int64  `json:"median_ms"`
	P95MS    int64  `json:"p95_ms"`
	WorstMS  int64  `json:"worst_ms"`
}

// PathReuse is how often one backing image:path was served and how many
// backing bytes its commands read - whether a bounded cache might pay off.
type PathReuse struct {
	Path  string `json:"path"`
	Opens int    `json:"opens"`
	Bytes int64  `json:"bytes"`
}

// SlowCommand names one slow command so it can be found in the serial
// transcript by session and sequence.
type SlowCommand struct {
	Session    string `json:"session"`
	Seq        int    `json:"seq"`
	Verb       string `json:"verb"`
	DurationMS int64  `json:"duration_ms"`
	Path       string `json:"path,omitempty"`
}

// rawEvent is the union of every event's fields, so one decode reads any
// line. Missing fields stay zero.
type rawEvent struct {
	Event   string `json:"event"`
	Session string `json:"session"`
	Client  string `json:"client"`
	Result  string `json:"result"`

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
	Served       []served `json:"served"`
	Marker       bool     `json:"marker"`

	CommandCount int   `json:"command_count"`
	BytesOut     int64 `json:"bytes_out"`

	Size        int64 `json:"size"`
	BytesSent   int64 `json:"bytes_sent"`
	Retransmits int   `json:"retransmits"`
}

// Summarize reads an events.jsonl stream and aggregates it. It tolerates a
// truncated final line (a crash mid-write): a line that won't parse is
// skipped, not fatal.
func Summarize(r io.Reader) (Summary, error) {
	var sum Summary

	type sessAgg struct {
		client      string
		result      string
		wall        int64
		active      int64
		commands    int
		bytesOut    int64
		stdoutBytes int64 // command stdout: the throughput numerator
	}
	sessions := map[string]*sessAgg{}
	var order []string
	get := func(id string) *sessAgg {
		s := sessions[id]
		if s == nil {
			s = &sessAgg{}
			sessions[id] = s
			order = append(order, id)
		}
		return s
	}

	verbDurations := map[string][]int64{}
	pathAgg := map[string]*PathReuse{}
	var pathOrder []string

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	// A crash can truncate the final line, but a malformed line anywhere
	// else is real corruption. Defer a parse error by one iteration: if
	// another line follows, the bad line was not last, so fail with its
	// number; if the file ends, it was the truncated tail and is tolerated.
	lineNo := 0
	var pendingErr error
	var pendingLine int
	for sc.Scan() {
		lineNo++
		if pendingErr != nil {
			return sum, fmt.Errorf("events.jsonl line %d: %w", pendingLine, pendingErr)
		}
		var e rawEvent
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			pendingErr, pendingLine = err, lineNo
			continue
		}
		switch e.Event {
		case "rsh_session_start":
			get(e.Session).client = e.Client

		case "rsh_session_end":
			s := get(e.Session)
			s.result = e.Result
			s.wall = e.DurationMS
			s.bytesOut = e.BytesOut

		case "inst_command_end":
			// A marker line usually wraps the real command it reports on -
			// "dd if=... ; ( status=$? ; ... IsDone ... )" - so the actual
			// install work lives in marker-flagged events. Count those under
			// the wrapped command's verb; only a wrapper with nothing
			// wrapped (the session-opening trap arm) is pure protocol
			// bookkeeping and excluded.
			verb := e.Verb
			if e.Marker {
				payload := markerPayload(e.Line)
				if payload == "" {
					break
				}
				verb = firstVerb(payload)
			}
			sum.Commands++
			agg := get(e.Session)
			agg.commands++
			agg.active += e.DurationMS
			agg.stdoutBytes += e.StdoutBytes
			sum.StdoutBytes += e.StdoutBytes
			sum.StdoutCalls += e.StdoutCalls
			sum.StderrBytes += e.StderrBytes
			sum.StderrCalls += e.StderrCalls
			sum.EFSReadBytes += e.EFSReadBytes
			sum.EFSReadCalls += e.EFSReadCalls
			switch e.Result {
			case "refused":
				sum.RefusedCommands++
			case "nonzero":
				sum.NonzeroCommands++
			}
			verbDurations[verb] = append(verbDurations[verb], e.DurationMS)
			sum.Slowest = append(sum.Slowest, SlowCommand{
				Session: e.Session, Seq: e.Seq, Verb: verb,
				DurationMS: e.DurationMS, Path: firstServedPath(e.Served),
			})
			for _, sv := range e.Served {
				key := sv.Image + ":/" + sv.ImagePath
				if sv.Image == "" {
					key = sv.TreePath
				}
				p := pathAgg[key]
				if p == nil {
					p = &PathReuse{Path: key}
					pathAgg[key] = p
					pathOrder = append(pathOrder, key)
				}
				p.Opens++
				// The event carries per-command EFS bytes, not per-file, so
				// bytes can only be attributed to a path when the command
				// served exactly one. A multi-file command counts opens only.
				if len(e.Served) == 1 {
					p.Bytes += e.EFSReadBytes
				}
			}

		case "tftp_transfer_end":
			sum.Transfers++
			sum.TFTPRetransmits += e.Retransmits
			sum.BytesServed += e.BytesSent
			switch e.Result {
			case "aborted":
				sum.AbortedTransfers++
			case "gaveup":
				sum.GaveupTransfers++
			}

		case "bootp_reply":
			if e.Result == "answered" {
				sum.BootpAnswered++
			}

		case "listener_exit":
			sum.ListenerExits++
		}
	}
	if err := sc.Err(); err != nil {
		return sum, err
	}
	// A pendingErr surviving the loop was the final line: a truncated tail,
	// tolerated.

	for _, id := range order {
		s := sessions[id]
		idle := s.wall - s.active
		if idle < 0 {
			idle = 0
		}
		sum.BytesServed += s.bytesOut
		// Throughput is command stdout bytes - the data the server actually
		// served - over active and wall time, so it matches the
		// active-command-time it is divided by. bytes_out is not used here:
		// it folds in stderr and the protocol echoes.
		var activeBps, wallBps int64
		if s.active > 0 {
			activeBps = s.stdoutBytes * 1000 / s.active
		}
		if s.wall > 0 {
			wallBps = s.stdoutBytes * 1000 / s.wall
		}
		sum.Sessions = append(sum.Sessions, SessionSummary{
			ID: id, Client: s.client, Result: s.result,
			WallMS: s.wall, ActiveMS: s.active, IdleMS: idle,
			Commands: s.commands, BytesOut: s.bytesOut,
			ActiveBps: activeBps, WallBps: wallBps,
		})
	}

	verbs := make([]string, 0, len(verbDurations))
	for v := range verbDurations {
		verbs = append(verbs, v)
	}
	sort.Strings(verbs)
	for _, v := range verbs {
		ds := verbDurations[v]
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		sum.Verbs = append(sum.Verbs, VerbLatency{
			Verb: v, Count: len(ds),
			MedianMS: percentile(ds, 0.50),
			P95MS:    percentile(ds, 0.95),
			WorstMS:  ds[len(ds)-1],
		})
	}

	for _, k := range pathOrder {
		sum.Paths = append(sum.Paths, *pathAgg[k])
	}
	sort.SliceStable(sum.Paths, func(i, j int) bool { return sum.Paths[i].Opens > sum.Paths[j].Opens })

	sort.SliceStable(sum.Slowest, func(i, j int) bool { return sum.Slowest[i].DurationMS > sum.Slowest[j].DurationMS })
	if len(sum.Slowest) > 10 {
		sum.Slowest = sum.Slowest[:10]
	}

	return sum, nil
}

func firstServedPath(sv []served) string {
	if len(sv) == 0 {
		return ""
	}
	if sv[0].Image != "" {
		return sv[0].Image + ":/" + sv[0].ImagePath
	}
	return sv[0].TreePath
}

// percentile returns the nearest-rank percentile of a sorted slice.
func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted))*p+0.999999) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// WriteText renders a short operator report: per-session timing, run-wide
// totals, and the slowest commands. It is the human half of the summary
// the server prints at shutdown and `trace summary` prints on demand.
func (s Summary) WriteText(w io.Writer) {
	for _, ss := range s.Sessions {
		fmt.Fprintf(w, "session %s (%s): wall %s  active %s  idle %s  commands %d  bytes_out %s\n",
			ss.ID, ss.Client, dur(ss.WallMS), dur(ss.ActiveMS), dur(ss.IdleMS), ss.Commands, bytesH(ss.BytesOut))
		fmt.Fprintf(w, "  throughput: active %s/s  wall %s/s\n", bytesH(ss.ActiveBps), bytesH(ss.WallBps))
	}
	fmt.Fprintf(w, "bytes served:        %s\n", bytesH(s.BytesServed))
	fmt.Fprintf(w, "commands:            %d\n", s.Commands)
	fmt.Fprintf(w, "refused commands:    %d\n", s.RefusedCommands)
	fmt.Fprintf(w, "nonzero commands:    %d\n", s.NonzeroCommands)
	fmt.Fprintf(w, "tftp transfers:      %d (retransmits %d, aborted %d, gaveup %d)\n", s.Transfers, s.TFTPRetransmits, s.AbortedTransfers, s.GaveupTransfers)
	fmt.Fprintf(w, "bootp answered:      %d\n", s.BootpAnswered)
	if s.ListenerExits > 0 {
		fmt.Fprintf(w, "listener exits:      %d  (UNEXPECTED - check the log)\n", s.ListenerExits)
	}
	if s.EFSReadCalls > 0 {
		fmt.Fprintf(w, "EFS reads:           %d (average %d B)\n", s.EFSReadCalls, s.EFSReadBytes/s.EFSReadCalls)
	}
	if s.StdoutCalls > 0 {
		fmt.Fprintf(w, "stdout writes:       %d (average %d B)\n", s.StdoutCalls, s.StdoutBytes/s.StdoutCalls)
	}
	if s.StderrCalls > 0 {
		fmt.Fprintf(w, "stderr writes:       %d (average %d B)\n", s.StderrCalls, s.StderrBytes/s.StderrCalls)
	}
	if len(s.Verbs) > 0 {
		fmt.Fprintln(w, "command latency by verb (count median/p95/worst):")
		for _, v := range s.Verbs {
			fmt.Fprintf(w, "  %-8s %4d  %s / %s / %s\n", v.Verb, v.Count, dur(v.MedianMS), dur(v.P95MS), dur(v.WorstMS))
		}
	}
	if len(s.Slowest) > 0 {
		fmt.Fprintln(w, "slowest commands:")
		for _, c := range s.Slowest {
			fmt.Fprintf(w, "  %s/%d %-8s %s  %s\n", c.Session, c.Seq, c.Verb, dur(c.DurationMS), c.Path)
		}
	}
}

func dur(ms int64) string { return (time.Duration(ms) * time.Millisecond).String() }

func bytesH(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(u), 0
	for m := n / u; m >= u; m /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
