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
}

// New starts a Writer draining to out with the given buffer depth.
func New(out io.Writer, depth int) *Writer {
	w := &Writer{ch: make(chan []byte, depth), out: out}
	go w.run()
	return w
}

func (w *Writer) run() {
	for buf := range w.ch {
		_, _ = w.out.Write(buf)
		if n := w.dropped.Swap(0); n > 0 {
			_, _ = fmt.Fprintf(w.out, "asynclog: dropped %d log lines while output was stalled\n", n)
		}
	}
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

// WriteSync writes straight to the output, bypassing the queue. For the last
// words before exit: a queued line is lost if the process dies before the
// drain goroutine runs, which makes an orderly shutdown look like a silent
// death.
func (w *Writer) WriteSync(s string) {
	_, _ = w.out.Write([]byte(s))
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
