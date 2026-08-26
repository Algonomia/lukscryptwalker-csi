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

// HostMount is one entry of the mount table: the device backing it and its
// filesystem type. Dev identifies the superblock, so it distinguishes two
// mounts of the same fs type at one path — which is the whole point.
type HostMount struct {
	Dev    string // maj:min
	FSType string
}

var (
	mountsMu     sync.Mutex
	mountsStacks map[string][]HostMount // mountpoint → mounts, mountinfo order (last is topmost)
	// Topmost mount per path; replaced together with mountsStacks so the two
	// views never disagree.
	mountsFlat      map[string]string
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
// One entry per path (the topmost); to ask whether a path is FULLY unmounted
// use HostMountDepth — a buried mount is absent here and from stat().
func HostMountsOK() (map[string]string, bool) {
	if _, ok := hostMountStacksOK(); !ok {
		return nil, false
	}
	mountsMu.Lock()
	defer mountsMu.Unlock()
	return mountsFlat, true
}

// HostMountStacksOK returns every mount at each path in the host namespace, in
// mountinfo order (last is topmost), and whether the table is trustworthy.
func HostMountStacksOK() (map[string][]HostMount, bool) {
	return hostMountStacksOK()
}

// HostMountDepth reports how many mounts are stacked at path in the host mount
// namespace, and whether the table could be read. Depth >1 means a buried
// mount still serves every process that opened it before the newer one.
func HostMountDepth(path string) (int, bool) {
	stacks, ok := hostMountStacksOK()
	if !ok {
		return 0, false
	}
	return len(stacks[path]), true
}

// InvalidateHostMounts drops the cached table so the next query re-reads
// /proc/1/mountinfo, for callers that just changed the mount tree.
func InvalidateHostMounts() {
	mountsMu.Lock()
	if !mountsReadAt.IsZero() {
		// Just past the TTL, not zero: a re-read that blocks must still serve
		// the previous table rather than report UNKNOWN.
		mountsReadAt = time.Now().Add(-mountsCacheTTL)
	}
	mountsMu.Unlock()
}

// hostMountStacksOK is the single reader behind every mount query.
func hostMountStacksOK() (map[string][]HostMount, bool) {
	mountsMu.Lock()
	fresh := mountsStacks != nil && time.Since(mountsReadAt) < mountsCacheTTL
	if fresh {
		cached := mountsStacks
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

	done := make(chan map[string][]HostMount, 1)
	go func() {
		m := readHostMounts()
		mountsMu.Lock()
		if m != nil {
			mountsStacks = m
			mountsFlat = flattenMountStacks(m)
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
func cachedMountsLocked() (map[string][]HostMount, bool) {
	if mountsStacks == nil {
		return nil, false
	}
	age := time.Since(mountsReadAt)
	if age < mountsMaxStale {
		return mountsStacks, true
	}
	klog.Errorf("Host mount table has been unreadable for %s (read blocked since %s): treating mount state as "+
		"UNKNOWN. Every mount check now fails closed — pods will not be published onto unverifiable mounts.",
		age.Round(time.Second), mountsBlockedAt.Format(time.RFC3339))
	return nil, false
}

// readHostMounts reads and parses the host mount table, falling back to our own
// namespace if the host view is unreadable.
func readHostMounts() map[string][]HostMount {
	data, err := os.ReadFile(hostMountInfo)
	if err != nil {
		if data, err = os.ReadFile(selfMountInfo); err != nil {
			klog.Warningf("Failed to read mount table: %v", err)
			return nil
		}
		klog.V(4).Infof("Host mount table unreadable, using our own namespace")
	}
	return parseMountStacks(string(data))
}

// ParseMountStacks parses /proc/<pid>/mountinfo content, for callers reading a
// namespace other than the host's — a container's own view of its mounts.
func ParseMountStacks(data string) map[string][]HostMount {
	return parseMountStacks(data)
}

// parseMountStacks maps mountpoint → every mount there, in mountinfo order.
// Keyed by path alone, stacked mounts collapse and a stale bind buried under a
// fresh one becomes unobservable.
// Format: ID PARENT MAJ:MIN ROOT MOUNTPOINT OPTIONS [OPTIONAL...] - FSTYPE SOURCE SUPEROPTS
func parseMountStacks(data string) map[string][]HostMount {
	out := make(map[string][]HostMount)
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mountPoint := unescapeMountField(fields[4])
		// Filesystem type is the first field after the " - " separator.
		for i := 5; i < len(fields)-1; i++ {
			if fields[i] == "-" {
				out[mountPoint] = append(out[mountPoint], HostMount{Dev: fields[2], FSType: fields[i+1]})
				break
			}
		}
	}
	return out
}

// parseMountInfo maps mountpoint → the fstype of the TOPMOST mount there.
func parseMountInfo(data string) map[string]string {
	return flattenMountStacks(parseMountStacks(data))
}

// flattenMountStacks keeps the topmost mount at each path — the one the kernel
// resolves for stat, open and umount.
func flattenMountStacks(stacks map[string][]HostMount) map[string]string {
	out := make(map[string]string, len(stacks))
	for path, mounts := range stacks {
		out[path] = mounts[len(mounts)-1].FSType
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
