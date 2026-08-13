package asynclog

import (
	"io"
	"testing"
	"time"
)

// blockingWriter (see asynclog_test.go) stands in for a container log pipe
// whose reader has stopped consuming: every write blocks until released.

// The exit paths dump diagnostics and then die. If that dump can block forever
// the process never exits — which is exactly how a wedged driver stayed Running
// and silent instead of restarting.
func TestWriteBoundedReturnsWhileOutputIsBlocked(t *testing.T) {
	w := &blockingWriter{release: make(chan struct{})}
	defer close(w.release)

	done := make(chan bool, 1)
	start := time.Now()
	go func() { done <- WriteBounded(w, "diagnostics\n", 100*time.Millisecond) }()

	select {
	case completed := <-done:
		if completed {
			t.Error("WriteBounded reported success against a writer that never returned")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("WriteBounded took %s to give up on a 100ms budget", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WriteBounded blocked on a stalled writer")
	}
}

func TestWriteBoundedReportsCompletion(t *testing.T) {
	if !WriteBounded(io.Discard, "line\n", time.Second) {
		t.Error("WriteBounded on a working writer reported failure")
	}
}

// A blocked drain silences every later line, self-monitor included. The stall
// has to be measurable, or the blackout reads as a frozen process.
func TestStalledForTracksBlockedOutput(t *testing.T) {
	w := &blockingWriter{release: make(chan struct{})}
	lw := New(w, 4)

	if got := lw.StalledFor(); got != 0 {
		t.Errorf("idle writer reported a stall of %s", got)
	}

	_, _ = lw.Write([]byte("first\n"))

	deadline := time.Now().Add(3 * time.Second)
	for lw.StalledFor() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if lw.StalledFor() == 0 {
		t.Fatal("a write blocked in the drain goroutine was not reported as a stall")
	}

	// Callers must keep making progress while it is stalled.
	for range 100 {
		if _, err := lw.Write([]byte("more\n")); err != nil {
			t.Fatalf("Write returned an error while output was stalled: %v", err)
		}
	}
	if lw.Dropped() == 0 {
		t.Error("lines past the buffer depth should be dropped and counted, not blocked on")
	}

	close(w.release)
	for lw.StalledFor() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if lw.StalledFor() != 0 {
		t.Error("stall was still reported after output resumed")
	}
}

func TestDefaultStalledForWithoutWriter(t *testing.T) {
	orig := defaultWriter.Load()
	defaultWriter.Store(nil)
	defer defaultWriter.Store(orig)

	if got := DefaultStalledFor(); got != 0 {
		t.Errorf("DefaultStalledFor with no writer = %s, want 0", got)
	}
}
