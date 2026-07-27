package driver

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

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

// ExpandBackingFile expands a backing file to the specified size in bytes
func ExpandBackingFile(filePath string, newSizeBytes int64) error {
	klog.Infof("Expanding backing file %s to %d bytes", filePath, newSizeBytes)

	// Already at size: skip. Re-fallocating a full-size file can fail ENOSPC on a
	// near-full host (XFS speculative preallocation) even though nothing needs doing.
	if fi, err := os.Stat(filePath); err == nil && fi.Size() >= newSizeBytes {
		klog.Infof("Backing file %s is already %d bytes (requested: %d)", filePath, fi.Size(), newSizeBytes)
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
			if ferr := ensureFreeSpace(filepath.Dir(filePath), delta); ferr != nil {
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
			if ferr := ensureFreeSpace(filepath.Dir(filePath), sizeBytes); ferr != nil {
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

// GetLocalPath determines the local path for a volume using environment variable
func GetLocalPath(volumeID string) string {
	// Use environment variable
	if envPath := os.Getenv("CSI_LOCAL_PATH"); envPath != "" {
		return filepath.Join(envPath, volumeID)
	}
	
	// Fallback to default
	return filepath.Join(DefaultLocalPath, volumeID)
}
