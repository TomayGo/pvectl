// Package log provides pvectl's dual-sink logger: a human-readable stream on
// stdout and a full transcript in /var/log/pvectl/<timestamp>.log on the node
// that ran the command.
//
// Every PVE API request is recorded through API() with a stable "API " prefix.
// That is deliberate: the API token starts life with --privsep 0, and the way
// it gets narrowed later is by grepping these lines for the methods and paths
// pvectl actually uses.
package log

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultDir is where per-run transcripts are written. Node-local on purpose:
// /etc/pve is shared but goes read-only when quorum is lost, which is exactly
// when we most need to keep writing.
const DefaultDir = "/var/log/pvectl"

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelWarn:
		return "WARN "
	case LevelError:
		return "ERROR"
	default:
		return "INFO "
	}
}

type Logger struct {
	mu       sync.Mutex
	file     *os.File
	Path     string
	term     io.Writer
	minLevel Level
	step     string
	// DryRun only affects the "would" wording helpers; it does not suppress logging.
	DryRun bool
}

// New opens a transcript file under dir. A failure to open the file is not
// fatal: pvectl must still work on a node with a full or read-only /var.
func New(dir string, verbose bool) *Logger {
	l := &Logger{term: os.Stdout, minLevel: LevelInfo}
	if verbose {
		l.minLevel = LevelDebug
	}
	if dir == "" {
		dir = DefaultDir
	}
	if err := os.MkdirAll(dir, 0o750); err == nil {
		p := filepath.Join(dir, time.Now().Format("20060102-150405")+".log")
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640); err == nil {
			l.file = f
			l.Path = p
		} else {
			fmt.Fprintf(os.Stderr, "pvectl: cannot open log file %s: %v\n", p, err)
		}
	} else {
		fmt.Fprintf(os.Stderr, "pvectl: cannot create log dir %s: %v\n", dir, err)
	}
	return l
}

// Discard returns a logger that writes nowhere. For tests.
func Discard() *Logger { return &Logger{term: io.Discard, minLevel: LevelError} }

func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
}

// SetStep tags subsequent lines with the plan step they belong to, so a
// transcript can be read back as a sequence of steps rather than a flat stream.
func (l *Logger) SetStep(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.step = name
}

func (l *Logger) write(lv Level, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.file != nil {
		prefix := ""
		if l.step != "" {
			prefix = "[" + l.step + "] "
		}
		fmt.Fprintf(l.file, "%s %s %s%s\n", now.Format("2006-01-02T15:04:05.000Z07:00"), lv, prefix, msg)
	}
	if lv < l.minLevel {
		return
	}
	// The terminal stream stays deliberately quieter than the transcript: a
	// timestamp on every line makes a long rolling reboot unreadable.
	switch lv {
	case LevelWarn:
		fmt.Fprintf(l.term, "  ! %s\n", msg)
	case LevelError:
		fmt.Fprintf(l.term, "  x %s\n", msg)
	case LevelDebug:
		fmt.Fprintf(l.term, "    %s\n", msg)
	default:
		fmt.Fprintf(l.term, "  %s\n", msg)
	}
}

func (l *Logger) Debugf(f string, a ...any) { l.write(LevelDebug, fmt.Sprintf(f, a...)) }
func (l *Logger) Infof(f string, a ...any)  { l.write(LevelInfo, fmt.Sprintf(f, a...)) }
func (l *Logger) Warnf(f string, a ...any)  { l.write(LevelWarn, fmt.Sprintf(f, a...)) }
func (l *Logger) Errorf(f string, a ...any) { l.write(LevelError, fmt.Sprintf(f, a...)) }

// Step announces a plan step on both sinks and makes it the current tag.
func (l *Logger) Step(name, desc string) {
	l.SetStep(name)
	l.mu.Lock()
	if l.file != nil {
		fmt.Fprintf(l.file, "%s INFO  === step %s: %s\n",
			time.Now().Format("2006-01-02T15:04:05.000Z07:00"), name, desc)
	}
	fmt.Fprintf(l.term, "\n==> %s: %s\n", name, desc)
	l.mu.Unlock()
}

// Result prints a terminal-only pass/fail marker; the transcript already has
// the detail lines that led to it.
func (l *Logger) Result(ok bool, name, detail string) {
	mark := "FAIL"
	if ok {
		mark = "ok"
	}
	l.write(levelFor(ok), fmt.Sprintf("[%s] %-28s %s", mark, name, detail))
}

func levelFor(ok bool) Level {
	if ok {
		return LevelInfo
	}
	return LevelError
}

// API records one PVE API call. Kept on its own line format so that
//
//	grep '^.* API ' /var/log/pvectl/*.log | awk '{print $4, $5}' | sort -u
//
// yields exactly the permission surface the token needs.
func (l *Logger) API(method, path string, status int, dur time.Duration, err error) {
	l.mu.Lock()
	if l.file != nil {
		line := fmt.Sprintf("%s INFO  API %s %s -> %d (%s)",
			time.Now().Format("2006-01-02T15:04:05.000Z07:00"), method, path, status, dur.Round(time.Millisecond))
		if err != nil {
			line += " err=" + err.Error()
		}
		fmt.Fprintln(l.file, line)
	}
	l.mu.Unlock()
	if l.minLevel <= LevelDebug {
		s := fmt.Sprintf("API %s %s -> %d (%s)", method, path, status, dur.Round(time.Millisecond))
		if err != nil {
			s += " err=" + err.Error()
		}
		fmt.Fprintf(l.term, "    %s\n", s)
	}
}

// Plain writes an unadorned line to the terminal and mirrors it to the
// transcript. For tables and reports that must not carry a log prefix.
func (l *Logger) Plain(f string, a ...any) {
	msg := fmt.Sprintf(f, a...)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		for _, ln := range strings.Split(msg, "\n") {
			fmt.Fprintf(l.file, "%s INFO  | %s\n", time.Now().Format("2006-01-02T15:04:05.000Z07:00"), ln)
		}
	}
	fmt.Fprintln(l.term, msg)
}
