package daemon

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Logger writes timestamped lines to a file and optionally to stderr.
//
// It satisfies drive.Logger so the Proton libraries log through the same
// sink, which matters when diagnosing a sync: an API warning and the sync
// line it explains end up adjacent instead of in different places.
type Logger struct {
	mu     sync.Mutex
	w      io.Writer
	f      *os.File
	stderr bool
}

// NewLogger opens a log file, falling back to stderr alone if it cannot.
func NewLogger(path string, alsoStderr bool) *Logger {
	l := &Logger{stderr: alsoStderr}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		l.w = os.Stderr
		l.stderr = false
		l.Warnf("could not open %s (%v); logging to stderr", path, err)
		return l
	}
	l.f = f
	l.w = f
	return l
}

// Close closes the log file.
func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
	}
}

func (l *Logger) write(level, format string, args ...interface{}) {
	line := fmt.Sprintf("%s %-5s %s\n",
		time.Now().Format("2006-01-02 15:04:05"), level, fmt.Sprintf(format, args...))

	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprint(l.w, line)
	if l.stderr && l.w != os.Stderr {
		fmt.Fprint(os.Stderr, line)
	}
}

// Infof logs an informational line.
func (l *Logger) Infof(format string, args ...interface{}) { l.write("INFO", format, args...) }

// Warnf logs a warning.
func (l *Logger) Warnf(format string, args ...interface{}) { l.write("WARN", format, args...) }

// Errorf logs an error.
func (l *Logger) Errorf(format string, args ...interface{}) { l.write("ERROR", format, args...) }

// Debugf logs a debug line. Kept quiet unless PDRIVE_DEBUG is set: the Proton
// libraries are chatty at this level.
func (l *Logger) Debugf(format string, args ...interface{}) {
	if os.Getenv("PDRIVE_DEBUG") == "" {
		return
	}
	l.write("DEBUG", format, args...)
}
