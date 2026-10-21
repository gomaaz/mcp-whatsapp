// Package client wraps go.mau.fi/whatsmeow and persists messages to the local
// message cache in internal/store. All logs are written to stderr.
package client

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/sealjay/mcp-whatsapp/internal/security"
)

var (
	logColors = map[string]string{
		"INFO":  "\033[36m",
		"WARN":  "\033[33m",
		"ERROR": "\033[31m",
	}
	logLevelToInt = map[string]int{
		"":      -1,
		"DEBUG": 0,
		"INFO":  1,
		"WARN":  2,
		"ERROR": 3,
	}
)

// logTimestampLayout prefixes every log line: UTC with millisecond precision
// and a "Z" suffix, so log times line up with the UTC times in tool output.
const logTimestampLayout = "2006-01-02T15:04:05.000Z07:00"

// logTime renders a message timestamp inside a log line the way tool output
// does: RFC 3339 in UTC ("2026-10-21T09:12:33Z").
func logTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// stderrLogger is a copy of whatsmeow's stdout logger that writes to an
// io.Writer (typically os.Stderr) instead of os.Stdout.
type stderrLogger struct {
	out   io.Writer
	mod   string
	color bool
	min   int
}

// NewStderrLogger returns a waLog.Logger that writes to os.Stderr. The API
// mirrors waLog.Stdout so callers can substitute it freely.
func NewStderrLogger(module string, level string, colorful bool) waLog.Logger {
	return &stderrLogger{
		out:   os.Stderr,
		mod:   module,
		color: colorful,
		min:   logLevelToInt[strings.ToUpper(level)],
	}
}

func (s *stderrLogger) outputf(level, msg string, args ...interface{}) {
	if logLevelToInt[level] < s.min {
		return
	}
	var colorStart, colorReset string
	if s.color {
		colorStart = logColors[level]
		if colorStart != "" {
			colorReset = "\033[0m"
		}
	}
	fmt.Fprintf(s.out, "%s%s [%s %s] %s%s\n",
		time.Now().UTC().Format(logTimestampLayout),
		colorStart, s.mod, level, fmt.Sprintf(msg, args...), colorReset,
	)
}

func (s *stderrLogger) Errorf(msg string, args ...interface{}) { s.outputf("ERROR", msg, args...) }
func (s *stderrLogger) Warnf(msg string, args ...interface{})  { s.outputf("WARN", msg, args...) }
func (s *stderrLogger) Infof(msg string, args ...interface{})  { s.outputf("INFO", msg, args...) }
func (s *stderrLogger) Debugf(msg string, args ...interface{}) { s.outputf("DEBUG", msg, args...) }

func (s *stderrLogger) Sub(mod string) waLog.Logger {
	return &stderrLogger{
		out:   s.out,
		mod:   fmt.Sprintf("%s/%s", s.mod, mod),
		color: s.color,
		min:   s.min,
	}
}

// redactingLogger passes every formatted line through Redactor.Line before
// handing it to the wrapped logger, and drops lines below min. It catches
// JIDs and phone numbers in text the call site did not redact: whatsmeow's
// own log lines and error values.
type redactingLogger struct {
	inner    waLog.Logger
	redactor *security.Redactor
	min      int
}

// newRedactingLogger wraps inner for non-debug operation. minLevel ("" for
// no extra filter, or "DEBUG", "INFO", "WARN", "ERROR") drops quieter lines
// before they reach inner. With r.Debug set, inner is returned unchanged.
func newRedactingLogger(inner waLog.Logger, r *security.Redactor, minLevel string) waLog.Logger {
	if r == nil || r.Debug {
		return inner
	}
	return &redactingLogger{inner: inner, redactor: r, min: logLevelToInt[strings.ToUpper(minLevel)]}
}

func (l *redactingLogger) emit(level string, out func(string, ...interface{}), msg string, args []interface{}) {
	if logLevelToInt[level] < l.min {
		return
	}
	out("%s", l.redactor.Line(fmt.Sprintf(msg, args...)))
}

func (l *redactingLogger) Errorf(msg string, args ...interface{}) {
	l.emit("ERROR", l.inner.Errorf, msg, args)
}
func (l *redactingLogger) Warnf(msg string, args ...interface{}) {
	l.emit("WARN", l.inner.Warnf, msg, args)
}
func (l *redactingLogger) Infof(msg string, args ...interface{}) {
	l.emit("INFO", l.inner.Infof, msg, args)
}
func (l *redactingLogger) Debugf(msg string, args ...interface{}) {
	l.emit("DEBUG", l.inner.Debugf, msg, args)
}

func (l *redactingLogger) Sub(mod string) waLog.Logger {
	return &redactingLogger{inner: l.inner.Sub(mod), redactor: l.redactor, min: l.min}
}
