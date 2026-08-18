package rclone

import (
	"testing"
	"time"
)

// Initialize runs under initMu, and RPCWithTimeout re-enters Initialize to
// ensure librclone is up. Any RPC issued from inside the once-block must
// therefore bypass that wrapper, or the driver deadlocks before it ever serves
// csi.sock — a total node-plugin outage that no pure-function test can see.
func TestInitializeDoesNotDeadlock(t *testing.T) {
	done := make(chan error, 1)
	go func() { done <- Initialize() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Initialize() = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Initialize() deadlocked: it holds initMu while something under it re-enters Initialize")
	}

	// Second call must be a cheap no-op, not a second lock acquisition that
	// blocks on whatever the first left held.
	done2 := make(chan error, 1)
	go func() { done2 <- Initialize() }()
	select {
	case err := <-done2:
		if err != nil {
			t.Fatalf("second Initialize() = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second Initialize() blocked")
	}
}
