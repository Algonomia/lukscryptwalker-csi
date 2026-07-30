// Package asynclog provides a never-blocking log writer. A stalled stderr
// (containerd's log pipe on frozen disk I/O) must not freeze every goroutine
// in the process through the global log mutex — lines are dropped and counted
// instead.
package asynclog

import (
	"fmt"
	"io"
	"sync/atomic"
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
			fmt.Fprintf(w.out, "asynclog: dropped %d log lines while output was stalled\n", n)
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

// Dropped returns the number of lines dropped since the last drain report.
func (w *Writer) Dropped() int64 { return w.dropped.Load() }
