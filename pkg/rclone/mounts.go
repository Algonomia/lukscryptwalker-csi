package rclone

import (
	"os"
	"strings"
	"sync"
	"time"

	"k8s.io/klog"
)

// Mount detection MUST use the host's mount table, not our own /proc/mounts.
// Our container's mount namespace can hold entries the host no longer has
// (e.g. after a sandbox is replaced under us): the driver then believes every
// volume is still mounted, the stale-mount checker finds nothing to repair,
// and consumers sit on dead or — worse — silently unmounted paths where the
// bind exposes the unencrypted directory underneath.
//
// hostPID gives us PID 1, whose mountinfo is the host mount namespace.
const (
	hostMountInfo = "/proc/1/mountinfo"
	selfMountInfo = "/proc/self/mountinfo"
)

const (
	// Reading a mount table takes the kernel's mount lock, which a wedged
	// umount on a dead FUSE holds indefinitely — so this read CAN block
	// forever. Cache it, single-flight it, and never let a caller wait on it:
	// a stale table is recoverable, a hung checker is not.
	mountsCacheTTL   = 5 * time.Second
	mountsReadBudget = 5 * time.Second
	// How long the last known table may be served once reads stop completing.
	// Past it it is reported UNKNOWN: a wedged reader otherwise pins a healthy
	// snapshot and every dead mount reads as live forever.
	mountsMaxStale = 90 * time.Second
)

var (
	mountsMu        sync.Mutex
	mountsCache     map[string]string
	mountsReadAt    time.Time
	mountsInFlight  bool
	mountsBlockedAt time.Time // when the currently in-flight read started
)

// HostMounts returns mountpoint → filesystem type as seen in the host mount
// namespace, or nil when the table cannot be established (see HostMountsOK).
func HostMounts() map[string]string {
	m, _ := HostMountsOK()
	return m
}

// HostMountsOK returns the host mount table and whether it is trustworthy,
// never blocking past mountsReadBudget. Callers taking a destructive or
// data-exposing decision must check ok — "unknown" is not "not mounted".
func HostMountsOK() (map[string]string, bool) {
	mountsMu.Lock()
	fresh := mountsCache != nil && time.Since(mountsReadAt) < mountsCacheTTL
	if fresh {
		cached := mountsCache
		mountsMu.Unlock()
		return cached, true
	}
	if mountsInFlight {
		cached, ok := cachedMountsLocked()
		mountsMu.Unlock()
		return cached, ok
	}
	mountsInFlight = true
	mountsBlockedAt = time.Now()
	mountsMu.Unlock()

	done := make(chan map[string]string, 1)
	go func() {
		m := readHostMounts()
		mountsMu.Lock()
		if m != nil {
			mountsCache = m
			mountsReadAt = time.Now()
		}
		mountsInFlight = false
		mountsMu.Unlock()
		done <- m
	}()

	select {
	case m := <-done:
		return m, m != nil
	case <-time.After(mountsReadBudget):
		mountsMu.Lock()
		cached, ok := cachedMountsLocked()
		mountsMu.Unlock()
		klog.Warningf("Reading the host mount table blocked for %s (a wedged umount holds the kernel mount "+
			"lock); serving the last known table with %d entries (trustworthy=%v)", mountsReadBudget, len(cached), ok)
		return cached, ok
	}
}

// cachedMountsLocked returns the cached table if young enough to act on.
func cachedMountsLocked() (map[string]string, bool) {
	if mountsCache == nil {
		return nil, false
	}
	age := time.Since(mountsReadAt)
	if age < mountsMaxStale {
		return mountsCache, true
	}
	klog.Errorf("Host mount table has been unreadable for %s (read blocked since %s): treating mount state as "+
		"UNKNOWN. Every mount check now fails closed — pods will not be published onto unverifiable mounts.",
		age.Round(time.Second), mountsBlockedAt.Format(time.RFC3339))
	return nil, false
}

// readHostMounts reads and parses the host mount table, falling back to our own
// namespace if the host view is unreadable.
func readHostMounts() map[string]string {
	data, err := os.ReadFile(hostMountInfo)
	if err != nil {
		if data, err = os.ReadFile(selfMountInfo); err != nil {
			klog.Warningf("Failed to read mount table: %v", err)
			return nil
		}
		klog.V(4).Infof("Host mount table unreadable, using our own namespace")
	}
	return parseMountInfo(string(data))
}

// parseMountInfo maps mountpoint → fstype from /proc/*/mountinfo content.
// Format: ID PARENT MAJ:MIN ROOT MOUNTPOINT OPTIONS [OPTIONAL...] - FSTYPE SOURCE SUPEROPTS
func parseMountInfo(data string) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mountPoint := unescapeMountField(fields[4])
		// Filesystem type is the first field after the " - " separator.
		for i := 5; i < len(fields)-1; i++ {
			if fields[i] == "-" {
				out[mountPoint] = fields[i+1]
				break
			}
		}
	}
	return out
}

// unescapeMountField decodes the octal escapes mountinfo uses for spaces and
// other special characters in paths.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}

// IsHostMountPoint reports whether path is a mount point in the host namespace.
func IsHostMountPoint(path string) bool {
	_, ok := HostMounts()[path]
	return ok
}

// IsHostFUSEMount reports whether path is served by a FUSE filesystem in the
// host namespace — i.e. one of our rclone mounts is actually live there.
// Fails closed: an unreadable mount table reports "not a FUSE mount".
func IsHostFUSEMount(path string) bool {
	isFUSE, _ := HostFUSEMountState(path)
	return isFUSE
}

// HostFUSEMountState reports whether path is FUSE in the host namespace, and
// whether that answer is trustworthy. Do not reconcile on known=false: absent
// and unknown look identical, and acting tears down healthy volumes.
func HostFUSEMountState(path string) (isFUSE, known bool) {
	mounts, known := HostMountsOK()
	if !known {
		return false, false
	}
	fsType, ok := mounts[path]
	return ok && strings.Contains(fsType, "fuse"), true
}

// HostMountsKnown reports whether the host mount table can currently be read.
func HostMountsKnown() bool {
	_, known := HostMountsOK()
	return known
}
