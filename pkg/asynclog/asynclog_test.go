package asynclog

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a goroutine-safe buffer the writer goroutine drains into.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// blockingWriter blocks all writes until released, like a stalled stderr pipe.
type blockingWriter struct{ release chan struct{} }

func (w *blockingWriter) Write(p []byte) (int, error) {
	<-w.release
	return len(p), nil
}

func TestPassthrough(t *testing.T) {
	out := &syncBuffer{}
	w := New(out, 16)
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "hello") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("line never drained: %q", out.String())
}

// A queued line must survive process exit, and the last words before exit
// must not depend on the drain goroutine getting scheduled.
func TestFlushAndWriteSync(t *testing.T) {
	out := &syncBuffer{}
	w := New(out, 64)

	for i := 0; i < 20; i++ {
		_, _ = w.Write([]byte("queued\n"))
	}
	w.Flush(2 * time.Second)
	if got := strings.Count(out.String(), "queued"); got != 20 {
		t.Errorf("Flush drained %d of 20 queued lines", got)
	}

	w.WriteSync("SHUTDOWN: bye\n")
	if !strings.Contains(out.String(), "SHUTDOWN: bye") {
		t.Error("WriteSync did not reach the output")
	}
}

func TestNeverBlocksAndCountsDrops(t *testing.T) {
	bw := &blockingWriter{release: make(chan struct{})}
	w := New(bw, 2)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			_, _ = w.Write([]byte("x\n"))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Write blocked on a stalled output — the exact failure this package exists to prevent")
	}

	if w.Dropped() == 0 {
		t.Error("expected dropped lines while output was stalled")
	}
	close(bw.release)
}
