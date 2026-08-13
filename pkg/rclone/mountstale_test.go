package rclone

import (
	"testing"
	"time"
)

// pinMountsCache installs a fake host mount table read that never completes,
// with the last successful read at the given age.
func pinMountsCache(t *testing.T, table map[string]string, age time.Duration) {
	t.Helper()
	mountsMu.Lock()
	origCache, origAt, origFlight, origBlocked := mountsCache, mountsReadAt, mountsInFlight, mountsBlockedAt
	mountsCache = table
	mountsReadAt = time.Now().Add(-age)
	mountsInFlight = true // a read is stuck on the kernel mount lock
	mountsBlockedAt = time.Now().Add(-age)
	mountsMu.Unlock()

	t.Cleanup(func() {
		mountsMu.Lock()
		mountsCache, mountsReadAt, mountsInFlight, mountsBlockedAt = origCache, origAt, origFlight, origBlocked
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
