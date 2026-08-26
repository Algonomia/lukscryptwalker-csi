package driver

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lukscryptwalker-csi/pkg/rclone"
)

// Two drains of one volume would mount two VFSes under the same name, after
// which vfs/stats can never answer for it again — so the slot must be claimed
// exactly once no matter how many kubelet retries and checker ticks pile up.
func TestStartBackgroundDrainClaimsSlotOnce(t *testing.T) {
	sm := NewS3SyncManager()
	t.Cleanup(func() { sm.finishBackgroundDrain("pvc-slot", true) })

	var claims atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if sm.startBackgroundDrain("pvc-slot") {
				claims.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := claims.Load(); got != 1 {
		t.Errorf("%d goroutines claimed the drain slot, want exactly 1", got)
	}
	if !sm.isBackgroundDraining("pvc-slot") {
		t.Error("slot was claimed but the volume does not report as draining")
	}
}

// The pending marker is the only record that this node holds a volume's only
// copy. Clearing it on a FAILED drain is what left stranded data invisible to
// the next driver instance, so failure has to keep it.
func TestFinishBackgroundDrainKeepsMarkerOnFailure(t *testing.T) {
	sm := NewS3SyncManager()
	t.Cleanup(func() { rclone.ClearDrainPending("pvc-failed") })

	if !sm.startBackgroundDrain("pvc-failed") {
		t.Fatal("could not claim the drain slot")
	}
	sm.finishBackgroundDrain("pvc-failed", false)

	if sm.isBackgroundDraining("pvc-failed") {
		t.Error("drain slot was not released")
	}
	if !sm.hasPendingDrain("pvc-failed") {
		t.Error("a failed drain cleared the pending marker — nothing afterwards can tell this node still " +
			"holds unuploaded writes")
	}

	// A later attempt that succeeds is what clears it.
	if !sm.startBackgroundDrain("pvc-failed") {
		t.Fatal("could not re-claim the drain slot after a failed drain")
	}
	sm.finishBackgroundDrain("pvc-failed", true)
	if sm.hasPendingDrain("pvc-failed") {
		t.Error("a completed drain left the pending marker behind")
	}
}

// waitForBackgroundDrain must not report success while a drain is still running.
func TestWaitForBackgroundDrainUnblocksOnFinish(t *testing.T) {
	sm := NewS3SyncManager()

	if ok := sm.waitForBackgroundDrain("pvc-idle", 0); !ok {
		t.Error("waiting on a volume with no drain should return immediately")
	}

	if !sm.startBackgroundDrain("pvc-waited") {
		t.Fatal("could not claim the drain slot")
	}
	done := make(chan bool, 1)
	go func() { done <- sm.waitForBackgroundDrain("pvc-waited", time.Minute) }()
	sm.finishBackgroundDrain("pvc-waited", true)
	if !<-done {
		t.Error("waiter reported a timeout after the drain finished")
	}
}
