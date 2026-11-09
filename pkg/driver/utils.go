package driver

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"k8s.io/klog"
)

// ensureFreeSpace returns an error unless dir's filesystem has at least needBytes available.
// Used to guard the sparse truncate fallback: a sparse backing file overcommits the host
// disk and later write I/O errors (ENOSPC) surface as a cryptic "mount: exit status 32"
// during ext4 journal replay.
func ensureFreeSpace(dir string, needBytes int64) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("statfs %s: %v", dir, err)
	}
	avail := int64(st.Bavail) * int64(st.Bsize)
	if avail < needBytes {
		return fmt.Errorf("%s has %d bytes free, need %d", dir, avail, needBytes)
	}
	return nil
}

// unreservedBytes returns how much of path's apparent size has no host blocks
// behind it. Writes into that range need free space the host may no longer have.
func unreservedBytes(path string) (int64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	if alloc := int64(st.Blocks) * 512; alloc < st.Size {
		return st.Size - alloc, nil
	}
	return 0, nil
}

// committedUnreservedBytes sums unreservedBytes over every backing file under
// root (layout root/<volumeID>/luks-<volumeID>.img).
func committedUnreservedBytes(root string) int64 {
	matches, _ := filepath.Glob(filepath.Join(root, "*", "luks-*.img"))
	var total int64
	for _, m := range matches {
		if n, err := unreservedBytes(m); err == nil {
			total += n
		}
	}
	return total
}

// ensureBackingSpace is ensureFreeSpace for backing files: space already promised
// to sparse backing files (including filePath's own holes) counts as used.
func ensureBackingSpace(filePath string, needBytes int64) error {
	dir := filepath.Dir(filePath)
	committed := committedUnreservedBytes(filepath.Dir(dir))
	if err := ensureFreeSpace(dir, needBytes+committed); err != nil {
		if committed > 0 {
			return fmt.Errorf("%v (includes %d bytes already promised to sparse backing files)", err, committed)
		}
		return err
	}
	return nil
}

// reserveBackingFile allocates any holes in an existing backing file, leaving its
// data untouched. It returns the bytes still unreserved afterwards.
func reserveBackingFile(filePath string) (int64, error) {
	holes, err := unreservedBytes(filePath)
	if err != nil || holes == 0 {
		return 0, err
	}
	fi, err := os.Stat(filePath)
	if err != nil {
		return holes, err
	}
	klog.Warningf("Backing file %s has %d of %d bytes unreserved on the host, allocating them", filePath, holes, fi.Size())
	out, ferr := exec.Command("fallocate", "-l", strconv.FormatInt(fi.Size(), 10), filePath).CombinedOutput()
	left, _ := unreservedBytes(filePath)
	if ferr != nil {
		return left, fmt.Errorf("fallocate %s: %v: %s", filePath, ferr, strings.TrimSpace(string(out)))
	}
	return left, nil
}

// backingSpaceHint explains a failed mount when the backing file is sparse and the
// host lacks the space for its holes; "" when that isn't the case.
func backingSpaceHint(filePath string) string {
	holes, err := unreservedBytes(filePath)
	if err != nil || holes == 0 {
		return ""
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(filePath), &st); err != nil {
		return ""
	}
	avail := int64(st.Bavail) * int64(st.Bsize)
	if avail >= holes {
		return ""
	}
	return fmt.Sprintf("backing file %s has %d bytes never allocated on the host and its filesystem has only %d free, "+
		"so writes into that range fail with EIO (e.g. ext4 journal replay): grow the host filesystem, "+
		"then the next stage allocates them (or run 'fallocate -l $(stat -c %%s %s) %s')", filePath, holes, avail, filePath, filePath)
}

// ExpandBackingFile expands a backing file to the specified size in bytes
func ExpandBackingFile(filePath string, newSizeBytes int64) error {
	klog.Infof("Expanding backing file %s to %d bytes", filePath, newSizeBytes)

	// Already at size: skip re-fallocating (it can ENOSPC on a near-full XFS host),
	// unless the file is sparse — the filesystem grow that follows writes into it.
	if fi, err := os.Stat(filePath); err == nil && fi.Size() >= newSizeBytes {
		klog.Infof("Backing file %s is already %d bytes (requested: %d)", filePath, fi.Size(), newSizeBytes)
		if left, rerr := reserveBackingFile(filePath); rerr != nil && left > 0 {
			if serr := ensureBackingSpace(filePath, 0); serr != nil {
				return fmt.Errorf("backing file %s is sparse and its holes cannot be backed by the host: %v; %v", filePath, rerr, serr)
			}
		}
		return nil
	}

	// First try with fallocate, which reserves real blocks (no later ENOSPC mid-write).
	cmd := exec.Command("fallocate", "-l", fmt.Sprintf("%d", newSizeBytes), filePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		klog.Warningf("fallocate failed for %s: %v, output: %s", filePath, err, strings.TrimSpace(string(out)))

		// Guard the sparse truncate fallback: require the additional space to actually exist
		// rather than silently overcommitting the host disk.
		var current int64
		if fi, statErr := os.Stat(filePath); statErr == nil {
			current = fi.Size()
		}
		if delta := newSizeBytes - current; delta > 0 {
			if ferr := ensureBackingSpace(filePath, delta); ferr != nil {
				return fmt.Errorf("refusing sparse expand of backing file: %v", ferr)
			}
		}

		cmd = exec.Command("truncate", "-s", fmt.Sprintf("%d", newSizeBytes), filePath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("failed to expand backing file with both fallocate and truncate: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}

	klog.Infof("Successfully expanded backing file %s to %d bytes", filePath, newSizeBytes)
	return nil
}

// CreateBackingFile creates a backing file with the specified size
func CreateBackingFile(filePath, size string) error {
	klog.Infof("Creating backing file %s with size %s", filePath, size)

	// Try fallocate first (works on ext4, xfs), which reserves real blocks so the volume
	// can't ENOSPC mid-write later.
	cmd := exec.Command("fallocate", "-l", size, filePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		klog.Warningf("fallocate failed for %s: %v, output: %s, trying truncate", filePath, err, strings.TrimSpace(string(output)))

		// Guard the sparse truncate fallback: require the space to actually exist rather than
		// silently overcommitting the host disk (which later surfaces as write I/O errors).
		if sizeBytes, perr := strconv.ParseInt(strings.TrimSpace(size), 10, 64); perr == nil {
			if ferr := ensureBackingSpace(filePath, sizeBytes); ferr != nil {
				return fmt.Errorf("refusing to create sparse backing file: %v", ferr)
			}
		}

		// Fallback to truncate (works on all filesystems, creates sparse file)
		cmd = exec.Command("truncate", "-s", size, filePath)
		output, err = cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to create backing file with truncate: %v, output: %s", err, strings.TrimSpace(string(output)))
		}
	}

	// Verify the file was created with correct size
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("failed to stat created backing file: %v", err)
	}

	klog.Infof("Successfully created backing file %s with size %d bytes", filePath, fileInfo.Size())
	return nil
}

// GenerateBackingFilePath creates a consistent backing file path for a given volume ID and local path
func GenerateBackingFilePath(localPath, volumeID string) string {
	return filepath.Join(localPath, fmt.Sprintf("luks-%s.img", volumeID))
}

// loopDevicesFor returns the loop devices currently backed by backingFile.
func loopDevicesFor(backingFile string) []string {
	out, err := runCmdBoundedOutput(30*time.Second, "losetup", "-j", backingFile)
	if err != nil {
		klog.V(4).Infof("losetup -j %s: %v", backingFile, err)
		return nil
	}
	return parseLosetupJ(out)
}

// parseLosetupJ extracts device paths from `losetup -j` output, whose lines
// look like "/dev/loop3: [64769]:1234 (/path/to/backing.img (deleted))".
func parseLosetupJ(out string) []string {
	var devices []string
	for _, line := range strings.Split(out, "\n") {
		dev, _, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && strings.HasPrefix(dev, "/dev/") {
			devices = append(devices, dev)
		}
	}
	return devices
}

// detachLoopDevicesFor releases every loop device still holding backingFile.
// Detaching one genuinely in use fails harmlessly; leaving it attached does not.
func detachLoopDevicesFor(backingFile string) {
	for _, dev := range loopDevicesFor(backingFile) {
		if err := runCmdBounded(30*time.Second, "losetup", "-d", dev); err != nil {
			klog.Warningf("Could not detach loop device %s for %s: %v", dev, backingFile, err)
			continue
		}
		klog.Infof("Detached leaked loop device %s (backing file %s)", dev, backingFile)
	}
}

// mountsBackedBy returns host mountpoints served by device, deepest first so a
// pod's bind is released before the staging mount it points at.
func mountsBackedBy(device string) []string {
	out, err := runCmdBoundedOutput(30*time.Second,
		"nsenter", "-t", "1", "-m", "findmnt", "-rn", "-o", "TARGET", "-S", device)
	if err != nil {
		// findmnt exits 1 when nothing matches, which is the common case.
		klog.V(4).Infof("findmnt -S %s: %v", device, err)
		return nil
	}
	return parseFindmntTargets(out)
}

// parseFindmntTargets turns `findmnt -rn -o TARGET` output into mountpoints.
func parseFindmntTargets(out string) []string {
	var targets []string
	for _, line := range strings.Split(out, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			targets = append(targets, unescapeMountTarget(t))
		}
	}
	sort.Slice(targets, func(i, j int) bool { return len(targets[i]) > len(targets[j]) })
	return targets
}

// unescapeMountTarget decodes the octal escapes findmnt's raw output uses.
func unescapeMountTarget(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	return strings.NewReplacer(`\x20`, " ", `\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// GetLocalPath determines the local path for a volume using environment variable
func GetLocalPath(volumeID string) string {
	// Use environment variable
	if envPath := os.Getenv("CSI_LOCAL_PATH"); envPath != "" {
		return filepath.Join(envPath, volumeID)
	}

	// Fallback to default
	return filepath.Join(DefaultLocalPath, volumeID)
}
