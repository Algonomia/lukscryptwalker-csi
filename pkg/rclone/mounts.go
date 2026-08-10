package rclone

import (
	"os"
	"strings"

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

// HostMounts returns mountpoint → filesystem type as seen in the host mount
// namespace, falling back to our own namespace if the host view is unreadable.
func HostMounts() map[string]string {
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
func IsHostFUSEMount(path string) bool {
	fsType, ok := HostMounts()[path]
	return ok && strings.Contains(fsType, "fuse")
}
