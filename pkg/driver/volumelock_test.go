package driver

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Two mounts of one volume overlapping is what registers a second VFS under the
// same name — after which every vfs/* RPC for that volume answers "more than one
// VFS active" until the driver restarts. The lock is that invariant.
func TestLockVolumeSerializesSameVolume(t *testing.T) {
	sm := NewS3SyncManager()

	var inCriticalSection, maxConcurrent atomic.Int32
	var wg sync.WaitGroup

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer sm.lockVolume("pvc-same")()

			n := inCriticalSection.Add(1)
			for {
				peak := maxConcurrent.Load()
				if n <= peak || maxConcurrent.CompareAndSwap(peak, n) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			inCriticalSection.Add(-1)
		}()
	}
	wg.Wait()

	if got := maxConcurrent.Load(); got != 1 {
		t.Errorf("%d mount operations for one volume ran concurrently, want 1", got)
	}
}

// Volumes must not queue behind each other: one slow mount would otherwise stall
// staging for every other volume on the node.
func TestLockVolumeDoesNotSerializeAcrossVolumes(t *testing.T) {
	sm := NewS3SyncManager()

	unlockA := sm.lockVolume("pvc-a")
	defer unlockA()

	got := make(chan struct{})
	go func() {
		defer sm.lockVolume("pvc-b")()
		close(got)
	}()

	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("locking one volume blocked an unrelated volume")
	}
}

func TestLockVolumeIsReentrantAcrossCalls(t *testing.T) {
	sm := NewS3SyncManager()

	// Same key, sequentially: the second acquisition must not deadlock on a
	// mutex the first one already released.
	sm.lockVolume("pvc-x")()
	done := make(chan struct{})
	go func() {
		sm.lockVolume("pvc-x")()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("re-acquiring a released volume lock deadlocked")
	}
}
