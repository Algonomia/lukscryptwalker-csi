package driver

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"k8s.io/klog"
)

// ExpandBackingFile expands a backing file to the specified size in bytes
func ExpandBackingFile(filePath string, newSizeBytes int64) error {
	klog.Infof("Expanding backing file %s to %d bytes", filePath, newSizeBytes)
	
	// First try with fallocate (more efficient for sparse files)
	cmd := exec.Command("fallocate", "-l", fmt.Sprintf("%d", newSizeBytes), filePath)
	if err := cmd.Run(); err != nil {
		// Fallback to truncate if fallocate fails
		cmd = exec.Command("truncate", "-s", fmt.Sprintf("%d", newSizeBytes), filePath)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to expand backing file with both fallocate and truncate: %v", err)
		}
	}
	
	klog.Infof("Successfully expanded backing file %s to %d bytes", filePath, newSizeBytes)
	return nil
}

// CreateBackingFile creates a backing file with the specified size
func CreateBackingFile(filePath, size string) error {
	cmd := exec.Command("fallocate", "-l", size, filePath)
	if err := cmd.Run(); err != nil {
		// Fallback to dd if fallocate is not available
		cmd = exec.Command("dd", "if=/dev/zero", "of="+filePath, "bs=1G", "count=1", "seek=0")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to create backing file with dd: %v", err)
		}
	}
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
