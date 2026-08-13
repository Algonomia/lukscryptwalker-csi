// Package asynclog provides a never-blocking log writer. A stalled stderr
// (containerd's log pipe on frozen disk I/O) must not freeze every goroutine
// in the process through the global log mutex — lines are dropped and counted
// instead.
package asynclog

import (
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// Writer buffers log lines to a background goroutine; Write never blocks.
type Writer struct {
	ch      chan []byte
	dropped atomic.Int64
	out     io.Writer
	// writeStartedAt is the unix-nano start of the in-flight write, 0 when idle.
	// A blocked write cannot be cancelled, so the blackout it causes is at least
	// made measurable: once the buffer fills every line is dropped and the
	// process goes silent while still running, which reads like a freeze.
	writeStartedAt atomic.Int64
}

// New starts a Writer draining to out with the given buffer depth.
func New(out io.Writer, depth int) *Writer {
	w := &Writer{ch: make(chan []byte, depth), out: out}
	go w.run()
	return w
}

// defaultWriter is the process's log writer, so components that cannot reach it
// directly can still ask whether logging is getting through.
var defaultWriter atomic.Pointer[Writer]

// SetDefault records w as the process's log writer.
func SetDefault(w *Writer) { defaultWriter.Store(w) }

// DefaultStalledFor returns how long the process has been unable to log, or 0.
// Report it somewhere other than the log, or it goes where every line is going.
func DefaultStalledFor() time.Duration {
	if w := defaultWriter.Load(); w != nil {
		return w.StalledFor()
	}
	return 0
}

func (w *Writer) run() {
	for buf := range w.ch {
		start := time.Now()
		w.writeStartedAt.Store(start.UnixNano())
		_, _ = w.out.Write(buf)
		w.writeStartedAt.Store(0)

		if stalled := time.Since(start); stalled > stallReportThreshold {
			_, _ = fmt.Fprintf(w.out, "asynclog: log output was blocked for %s (stalled stderr/container log pipe); "+
				"any gap in these logs before this line is missing output, not an idle driver\n", stalled.Round(time.Second))
		}
		if n := w.dropped.Swap(0); n > 0 {
			_, _ = fmt.Fprintf(w.out, "asynclog: dropped %d log lines while output was stalled\n", n)
		}
	}
}

// stallReportThreshold is how long a write must block to be worth reporting.
const stallReportThreshold = 5 * time.Second

// StalledFor returns how long the in-flight write has been blocked, 0 if none.
func (w *Writer) StalledFor() time.Duration {
	started := w.writeStartedAt.Load()
	if started == 0 {
		return 0
	}
	return time.Since(time.Unix(0, started))
}

// Write enqueues the line, dropping it (counted) when the buffer is full.
func (w *Writer) Write(p []byte) (int, error) {
	buf := make([]byte, len(p))
	copy(buf, p)
	select {
	case w.ch <- buf:
	default:
		w.dropped.Add(1)
	}
	return len(p), nil
}

// WriteSync writes straight to the output, bypassing the queue, giving up after
// timeout. For last words before exit: a queued line dies with the process, but
// a stalled pipe must not turn "log and exit" into "never exit".
func (w *Writer) WriteSync(s string) {
	WriteBounded(w.out, s, syncWriteTimeout)
}

// syncWriteTimeout bounds a direct write to a possibly-stalled output.
const syncWriteTimeout = 5 * time.Second

// WriteBounded writes s to out, returning after timeout either way and
// reporting whether it completed. For paths that must make progress even when
// the log pipe is blocked — above all those that dump diagnostics and exit.
func WriteBounded(out io.Writer, s string, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		_, _ = out.Write([]byte(s))
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Flush waits (up to timeout) for queued lines to drain, so log output
// survives process exit.
func (w *Writer) Flush(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for len(w.ch) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

// Dropped returns the number of lines dropped since the last drain report.
func (w *Writer) Dropped() int64 { return w.dropped.Load() }
