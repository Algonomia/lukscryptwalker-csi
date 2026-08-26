package rclone

import (
	"testing"
	"time"
)

// pinMountsCache installs a fake host mount table read that never completes,
// with the last successful read at the given age.
func pinMountsCache(t *testing.T, table map[string]string, age time.Duration) {
	t.Helper()
	stacks := make(map[string][]HostMount, len(table))
	for path, fsType := range table {
		stacks[path] = []HostMount{{Dev: "0:1", FSType: fsType}}
	}

	mountsMu.Lock()
	origStacks, origFlat := mountsStacks, mountsFlat
	origAt, origFlight, origBlocked := mountsReadAt, mountsInFlight, mountsBlockedAt
	mountsStacks, mountsFlat = stacks, table
	mountsReadAt = time.Now().Add(-age)
	mountsInFlight = true // a read is stuck on the kernel mount lock
	mountsBlockedAt = time.Now().Add(-age)
	mountsMu.Unlock()

	t.Cleanup(func() {
		mountsMu.Lock()
		mountsStacks, mountsFlat = origStacks, origFlat
		mountsReadAt, mountsInFlight, mountsBlockedAt = origAt, origFlight, origBlocked
		mountsMu.Unlock()
	})
}

// pinMountsStacks installs a fresh host mount table, stacks and all.
func pinMountsStacks(t *testing.T, stacks map[string][]HostMount) {
	t.Helper()
	mountsMu.Lock()
	origStacks, origFlat, origAt := mountsStacks, mountsFlat, mountsReadAt
	mountsStacks, mountsFlat = stacks, flattenMountStacks(stacks)
	mountsReadAt = time.Now()
	mountsMu.Unlock()

	t.Cleanup(func() {
		mountsMu.Lock()
		mountsStacks, mountsFlat, mountsReadAt = origStacks, origFlat, origAt
		mountsMu.Unlock()
	})
}

var fuseTable = map[string]string{"/var/lib/kubelet/plugins/x/globalmount": "fuse.rclone"}

// While the table is merely a little stale, serving it is better than blocking.
func TestRecentlyStaleMountTableIsStillUsable(t *testing.T) {
	pinMountsCache(t, fuseTable, mountsCacheTTL+time.Second)

	mounts, known := HostMountsOK()
	if !known {
		t.Fatal("a table a few seconds old was reported as unknown")
	}
	if len(mounts) != 1 {
		t.Fatalf("got %d entries, want 1", len(mounts))
	}
	if !IsHostFUSEMount("/var/lib/kubelet/plugins/x/globalmount") {
		t.Error("the FUSE mount in the cached table was not reported")
	}
}

// Once reads have been blocked long enough, the snapshot is a lie: it was taken
// while the volumes were healthy and would report every dead mount as live
// forever. Callers must be told the state is unknown, not handed the snapshot.
func TestPermanentlyBlockedMountTableBecomesUnknown(t *testing.T) {
	pinMountsCache(t, fuseTable, mountsMaxStale+time.Minute)

	if mounts, known := HostMountsOK(); known || mounts != nil {
		t.Errorf("stale-beyond-limit table reported as known (%d entries)", len(mounts))
	}
	if HostMountsKnown() {
		t.Error("HostMountsKnown must be false while reads are wedged")
	}

	// Every derived check has to fail closed, so nothing gets published onto a
	// mount we cannot verify.
	if IsHostMountPoint("/var/lib/kubelet/plugins/x/globalmount") {
		t.Error("IsHostMountPoint must fail closed on an unknown table")
	}
	if IsHostFUSEMount("/var/lib/kubelet/plugins/x/globalmount") {
		t.Error("IsHostFUSEMount must fail closed on an unknown table")
	}
	isFUSE, known := HostFUSEMountState("/var/lib/kubelet/plugins/x/globalmount")
	if isFUSE || known {
		t.Errorf("HostFUSEMountState = (%v, %v), want (false, false)", isFUSE, known)
	}
}

// "Not in the table" and "table unreadable" must stay distinguishable.
func TestAbsentMountIsNotUnknown(t *testing.T) {
	pinMountsCache(t, map[string]string{"/other": "ext4"}, time.Second*6)

	isFUSE, known := HostFUSEMountState("/var/lib/kubelet/plugins/x/globalmount")
	if isFUSE {
		t.Error("reported a FUSE mount that is not in the table")
	}
	if !known {
		t.Error("a readable table that simply lacks the path must still be known")
	}
}
