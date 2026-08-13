package rclone

import (
	"strings"
	"testing"
	"time"
)

// registerInFlight fakes a librclone call that started ago and never returned.
func registerInFlight(t *testing.T, method string, ago time.Duration) {
	t.Helper()
	rpcMu.Lock()
	rpcSeq++
	id := rpcSeq
	rpcInFlight[id] = inFlightRPC{method: method, started: time.Now().Add(-ago), budget: RPCDefaultTimeout}
	rpcMu.Unlock()
	t.Cleanup(func() {
		rpcMu.Lock()
		delete(rpcInFlight, id)
		rpcMu.Unlock()
	})
}

func withFakeAbort(t *testing.T) *string {
	t.Helper()
	reason := ""
	orig := rpcAbort
	rpcAbort = func(r string) { reason = r }
	t.Cleanup(func() { rpcAbort = orig })
	return &reason
}

// A mount call stuck past the threshold holds rclone's global mount mutex: no
// volume on the node can be mounted or unmounted again, so the process must go.
func TestStuckMountCallIsFatal(t *testing.T) {
	reason := withFakeAbort(t)
	registerInFlight(t, "mount/unmount", rpcStuckFatalAfter+time.Minute)

	checkStuckRPCs(time.Now())

	if *reason == "" {
		t.Fatal("a mount call stuck past the fatal threshold did not abort the process")
	}
	if !strings.Contains(*reason, "mount/unmount") {
		t.Errorf("abort reason does not name the stuck call: %q", *reason)
	}
}

// Everything else stuck is bad but survivable — other volumes keep working, so
// killing the plugin would cost more than it saves.
func TestStuckNonMountCallIsNotFatal(t *testing.T) {
	reason := withFakeAbort(t)
	registerInFlight(t, "vfs/stats", rpcStuckFatalAfter+time.Minute)

	checkStuckRPCs(time.Now())

	if *reason != "" {
		t.Errorf("a stuck vfs/stats must not kill the plugin, but aborted with: %q", *reason)
	}
}

func TestSlowCallsAreNotStuck(t *testing.T) {
	reason := withFakeAbort(t)
	registerInFlight(t, "mount/mount", rpcStuckFatalAfter-time.Minute)

	checkStuckRPCs(time.Now())

	if *reason != "" {
		t.Errorf("a call still within its threshold aborted: %q", *reason)
	}
	if got := stuckRPCs(time.Now()); len(got) != 0 {
		t.Errorf("stuckRPCs reported %d stuck calls, want 0", len(got))
	}
}

// Purging a large volume legitimately runs for a long time. Its own budget is
// the yardstick, not the fatal floor, or the log fills with false alarms.
func TestLongBudgetCallIsNotStuck(t *testing.T) {
	rpcMu.Lock()
	rpcSeq++
	id := rpcSeq
	rpcInFlight[id] = inFlightRPC{
		method:  "operations/purge",
		started: time.Now().Add(-20 * time.Minute),
		budget:  30 * time.Minute,
	}
	rpcMu.Unlock()
	defer func() {
		rpcMu.Lock()
		delete(rpcInFlight, id)
		rpcMu.Unlock()
	}()

	if got := stuckRPCs(time.Now()); len(got) != 0 {
		t.Errorf("a call still inside its 30m budget was reported stuck: %+v", got)
	}
}

func TestHoldsMountLock(t *testing.T) {
	for _, m := range []string{"mount/mount", "mount/unmount", "mount/listmounts"} {
		if !holdsMountLock(m) {
			t.Errorf("%s serializes on rclone's mount mutex but was not treated as such", m)
		}
	}
	for _, m := range []string{"vfs/stats", "vfs/list", "core/stats", "config/create", "operations/purge"} {
		if holdsMountLock(m) {
			t.Errorf("%s does not hold the mount mutex but was treated as fatal-if-stuck", m)
		}
	}
}

// A timed-out call must release its caller and be recognisable, so callers can
// distinguish "librclone is wedged" from "librclone said no".
func TestCallLibrcloneTimeoutReleasesCaller(t *testing.T) {
	before := len(stuckRPCs(time.Now()))

	done := make(chan error, 1)
	go func() {
		// core/version against an uninitialised librclone returns promptly;
		// what matters here is the timeout path, so use an impossible budget.
		_, _, err := callLibrclone("vfs/list", "{}", time.Nanosecond)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil && !IsRPCTimeout(err) {
			t.Fatalf("unexpected error: %v", err)
		}
		if err == nil {
			t.Skip("call completed before the timeout fired")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("callLibrclone did not release its caller")
	}

	if after := len(stuckRPCs(time.Now())); after < before {
		t.Errorf("in-flight bookkeeping went backwards: %d -> %d", before, after)
	}
}
