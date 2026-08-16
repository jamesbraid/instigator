// Package logging is instigator's leveled log output: ERROR/WARN/INFO/
// DEBUG lines with ISO8601 timestamps, built on log/slog. It exists
// because the flat, single-level log this project started with hid a
// real bug: an rsh command instcmd refused ("fgrep: not supported")
// went only to the client's own stderr, never to our side, so serve.log
// stayed silent about it through a live install failure. A refusal now
// has a level - ERROR - that's visible in the server's own log by
// default, not something a client can swallow.
//
// Every existing call site across bootp/tftp/rcmd/instcmd/serve already
// builds a formatted message with fmt-style verbs; Logger keeps that
// shape (Debugf/Infof/Warnf/Errorf) rather than moving every call site
// to slog's structured key-value API, which would be a much bigger
// change for no benefit here - nothing in this project queries its own
// logs as structured data.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
)

// Level is a log severity. The four constants below are the only
// levels instigator uses.
type Level = slog.Level

const (
	LevelDebug = slog.LevelDebug // per-packet/per-block decode detail, -v only
	LevelInfo  = slog.LevelInfo  // normal serving: requests answered, transfers made
	LevelWarn  = slog.LevelWarn  // a request refused or malformed on the client's end
	LevelError = slog.LevelError // this side failed to do something it should have
)

// Logger provides leveled, printf-style logging to one destination.
// The zero value is not usable; use New. A nil *Logger is safe to call
// (every method is a no-op), matching the old Logf-unset-means-silent
// convention every caller and test already relies on.
type Logger struct {
	slog *slog.Logger
}

// New builds a Logger writing one line per record to w: an ISO8601
// timestamp (RFC3339 with the local zone offset, e.g.
// 2026-08-16T16:07:38-07:00), the level, then the formatted message.
// min is the lowest level actually written; records below it cost
// nothing beyond a level comparison, since Debugf/Infof/etc. check it
// before formatting their arguments.
func New(w io.Writer, min Level) *Logger {
	return &Logger{slog: slog.New(&lineHandler{w: w, min: min})}
}

func (l *Logger) Debugf(format string, args ...any) { l.logf(LevelDebug, format, args) }
func (l *Logger) Infof(format string, args ...any)  { l.logf(LevelInfo, format, args) }
func (l *Logger) Warnf(format string, args ...any)  { l.logf(LevelWarn, format, args) }
func (l *Logger) Errorf(format string, args ...any) { l.logf(LevelError, format, args) }

// Enabled reports whether a message at level would actually be
// written. A hot path that builds expensive-to-format arguments (a
// per-packet decode, say) should check this before doing that work,
// rather than relying on Debugf to discard the message afterward.
func (l *Logger) Enabled(level Level) bool {
	if l == nil {
		return false
	}
	return l.slog.Enabled(context.Background(), level)
}

func (l *Logger) logf(level Level, format string, args []any) {
	if !l.Enabled(level) {
		return
	}
	l.slog.Log(context.Background(), level, fmt.Sprintf(format, args...))
}

// lineHandler is a minimal slog.Handler: one plain line per record,
// "<ISO8601> <LEVEL> <message>", no key=value structure. Every call
// site already composes its own message; a structured handler would
// just add noise here without anything consuming the structure.
type lineHandler struct {
	w   io.Writer
	min Level
	mu  sync.Mutex
}

func (h *lineHandler) Enabled(_ context.Context, level slog.Level) bool { return level >= h.min }

func (h *lineHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := fmt.Fprintf(h.w, "%s %-5s %s\n", r.Time.Format(time.RFC3339), r.Level.String(), r.Message)
	return err
}

// WithAttrs and WithGroup exist to satisfy slog.Handler; this package
// never attaches structured attrs or groups, so both are no-ops.
func (h *lineHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *lineHandler) WithGroup(_ string) slog.Handler      { return h }
