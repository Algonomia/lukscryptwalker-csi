package rclone

import (
	"testing"
	"time"
)

// The note is what makes Mount watch a fresh mount at a path whose previous
// session is still tearing down. Losing it means reporting a mount healthy
// moments before rclone's unmount-by-path finalizer takes it away.
func TestPendingFinalizerNote(t *testing.T) {
	const path = "/var/lib/kubelet/plugins/kubernetes.io/csi/x/globalmount"
	t.Cleanup(func() {
		pendingFinalizersMu.Lock()
		pendingFinalizers = map[string]time.Time{}
		pendingFinalizersMu.Unlock()
	})

	if takePendingFinalizer(path) {
		t.Error("a path nothing tore down has no pending finalizer")
	}

	notePendingFinalizer(path)
	if !takePendingFinalizer(path) {
		t.Error("a torn-down session must be reported to the next mount")
	}
	// Consumed: the settle window it asked for has been served.
	if takePendingFinalizer(path) {
		t.Error("the note must not survive being taken")
	}

	// A note older than the window a finalizer could plausibly fire in is not
	// worth a settle window on every later mount.
	pendingFinalizersMu.Lock()
	pendingFinalizers[path] = time.Now().Add(-pendingFinalizerTTL - time.Second)
	pendingFinalizersMu.Unlock()
	if takePendingFinalizer(path) {
		t.Error("a note past its TTL must not force a settle window")
	}

	// Stale notes for other paths are reclaimed, not accumulated: volumes come
	// and go for the life of the process.
	pendingFinalizersMu.Lock()
	pendingFinalizers["/stale"] = time.Now().Add(-pendingFinalizerTTL - time.Second)
	pendingFinalizersMu.Unlock()
	takePendingFinalizer(path)
	pendingFinalizersMu.Lock()
	_, kept := pendingFinalizers["/stale"]
	pendingFinalizersMu.Unlock()
	if kept {
		t.Error("expired notes for other paths must be dropped")
	}
}
