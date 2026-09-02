// Package logging is instigator's leveled, printf-style log output: one line
// per record - an RFC3339 timestamp, the level (DEBUG/INFO/WARN/ERROR), then
// the formatted message. A nil *Logger is a silent no-op, so an unset logger
// costs nothing and every call site can stay unconditional.
package logging

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// Level is a log severity; the four constants are the only levels instigator
// uses, ordered least to most severe.
type Level int

const (
	LevelDebug Level = iota // per-packet/per-block decode detail, -v only
	LevelInfo               // normal serving: requests answered, transfers made
	LevelWarn               // a request refused or malformed on the client's end
	LevelError              // this side failed to do something it should have
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "?"
	}
}

// Logger writes leveled, printf-style lines to one destination. A nil *Logger
// is safe to call - every method is a no-op - matching the unset-means-silent
// convention callers and tests rely on.
type Logger struct {
	w   io.Writer
	min Level
	mu  sync.Mutex
}

// New builds a Logger writing to w. min is the lowest level written; records
// below it cost only a level comparison, since the level methods check Enabled
// before formatting their arguments.
func New(w io.Writer, min Level) *Logger {
	return &Logger{w: w, min: min}
}

func (l *Logger) Debugf(format string, args ...any) { l.logf(LevelDebug, format, args) }
func (l *Logger) Infof(format string, args ...any)  { l.logf(LevelInfo, format, args) }
func (l *Logger) Warnf(format string, args ...any)  { l.logf(LevelWarn, format, args) }
func (l *Logger) Errorf(format string, args ...any) { l.logf(LevelError, format, args) }

// Enabled reports whether a message at level would be written. A hot path that
// builds expensive-to-format arguments should check this before doing that
// work, rather than relying on the level method to discard the message after.
func (l *Logger) Enabled(level Level) bool {
	return l != nil && level >= l.min
}

func (l *Logger) logf(level Level, format string, args []any) {
	if !l.Enabled(level) {
		return
	}
	msg := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, "%s %-5s %s\n", time.Now().Format(time.RFC3339), level, msg)
}
