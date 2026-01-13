package rclone

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lukscryptwalker-csi/pkg/luks"
	"k8s.io/klog"
)

const (
	// VFSCacheBasePath is the base path for the encrypted VFS cache
	// Mount directly at rclone's default cache location
	VFSCacheBasePath = "/root/.cache/rclone"
	// VFSCacheMapperName is the LUKS mapper name for VFS cache
	VFSCacheMapperName = "luks-vfs-cache"
	// VFSCacheBackingFile is the backing file for VFS cache LUKS volume
	VFSCacheBackingFile = "/var/lib/lukscrypt-vfs-cache.luks"
)

// SetupVFSCache creates and mounts an encrypted LUKS volume for VFS cache
// Returns the mount path or empty string if setup fails
func SetupVFSCache(sizeStr string, passphrase string) (string, error) {
	// Mount directly at the rclone cache base path
	mountPath := VFSCacheBasePath

	// Parse size string to bytes (default to 20GB if parsing fails)
	cacheSize := int64(20 * 1024 * 1024 * 1024) // 20GB default
	if sizeStr != "" {
		if parsed, err := parseSizeToBytes(sizeStr); err == nil && parsed > 0 {
			cacheSize = parsed
		} else {
			klog.Warningf("Failed to parse VFS cache size '%s', using default 20GB: %v", sizeStr, err)
		}
	}

	klog.Infof("Setting up encrypted VFS cache: size=%d bytes, backing=%s, mount=%s",
		cacheSize, VFSCacheBackingFile, mountPath)

	// Check if already mounted
	if isVFSCacheMounted(mountPath) {
		klog.Infof("VFS cache already mounted at %s", mountPath)
		return mountPath, nil
	}

	// Clean up any stale state
	cleanupStaleVFSCache(mountPath)

	// Create directories
	if err := os.MkdirAll(filepath.Dir(VFSCacheBackingFile), 0700); err != nil {
		return "", fmt.Errorf("failed to create VFS cache directory: %w", err)
	}
	if err := os.MkdirAll(mountPath, 0700); err != nil {
		return "", fmt.Errorf("failed to create VFS cache mount directory: %w", err)
	}

	luksManager := luks.NewLUKSManager()

	// Create backing file if it doesn't exist
	if _, err := os.Stat(VFSCacheBackingFile); os.IsNotExist(err) {
		klog.Infof("Creating VFS cache backing file %s with size %d bytes", VFSCacheBackingFile, cacheSize)

		// Create sparse file
		f, err := os.Create(VFSCacheBackingFile)
		if err != nil {
			return "", fmt.Errorf("failed to create VFS cache backing file: %w", err)
		}
		if err := f.Truncate(cacheSize); err != nil {
			_ = f.Close()
			_ = os.Remove(VFSCacheBackingFile)
			return "", fmt.Errorf("failed to set VFS cache backing file size: %w", err)
		}
		_ = f.Close()

		// Setup loop device
		loopDevice, err := setupLoopDevice(VFSCacheBackingFile)
		if err != nil {
			_ = os.Remove(VFSCacheBackingFile)
			return "", fmt.Errorf("failed to setup loop device: %w", err)
		}

		// Format with LUKS
		if err := luksManager.FormatAndOpenLUKS(loopDevice, VFSCacheMapperName, passphrase); err != nil {
			_ = detachLoopDevice(loopDevice)
			_ = os.Remove(VFSCacheBackingFile)
			return "", fmt.Errorf("failed to format LUKS VFS cache: %w", err)
		}

		// Format the mapped device with ext4
		mappedDevice := luksManager.GetMappedDevicePath(VFSCacheMapperName)
		if err := formatExt4(mappedDevice); err != nil {
			_ = luksManager.CloseLUKS(VFSCacheMapperName)
			_ = detachLoopDevice(loopDevice)
			_ = os.Remove(VFSCacheBackingFile)
			return "", fmt.Errorf("failed to format VFS cache filesystem: %w", err)
		}
	} else {
		// Backing file exists, just open it
		loopDevice, err := setupLoopDevice(VFSCacheBackingFile)
		if err != nil {
			return "", fmt.Errorf("failed to setup loop device: %w", err)
		}

		if err := luksManager.OpenLUKS(loopDevice, VFSCacheMapperName, passphrase); err != nil {
			_ = detachLoopDevice(loopDevice)
			return "", fmt.Errorf("failed to open LUKS VFS cache: %w", err)
		}
	}

	// Mount the encrypted cache
	mappedDevice := luksManager.GetMappedDevicePath(VFSCacheMapperName)
	if err := mountFilesystem(mappedDevice, mountPath); err != nil {
		_ = luksManager.CloseLUKS(VFSCacheMapperName)
		return "", fmt.Errorf("failed to mount VFS cache filesystem: %w", err)
	}

	klog.Infof("Successfully set up encrypted VFS cache at %s", mountPath)
	return mountPath, nil
}

// TeardownVFSCache unmounts and closes the encrypted VFS cache
func TeardownVFSCache() error {
	mountPath := VFSCacheBasePath
	luksManager := luks.NewLUKSManager()

	klog.Infof("Tearing down encrypted VFS cache")

	// Unmount the filesystem
	if err := unmountFilesystem(mountPath); err != nil {
		klog.Warningf("Failed to unmount VFS cache filesystem: %v", err)
	}

	// Close LUKS
	if err := luksManager.CloseLUKS(VFSCacheMapperName); err != nil {
		klog.Warningf("Failed to close LUKS VFS cache: %v", err)
	}

	// Find and detach loop device
	loopDevice, err := findLoopDevice(VFSCacheBackingFile)
	if err == nil && loopDevice != "" {
		_ = detachLoopDevice(loopDevice)
	}

	klog.Infof("Successfully tore down encrypted VFS cache")
	return nil
}

// isVFSCacheMounted checks if the VFS cache is already mounted
func isVFSCacheMounted(mountPath string) bool {
	cmd := exec.Command("mountpoint", "-q", mountPath)
	return cmd.Run() == nil
}

// cleanupStaleVFSCache cleans up any stale VFS cache state
func cleanupStaleVFSCache(mountPath string) {
	luksManager := luks.NewLUKSManager()
	mappedDevice := luksManager.GetMappedDevicePath(VFSCacheMapperName)

	// Unmount if stale mount exists
	if cmd := exec.Command("mountpoint", "-q", mountPath); cmd.Run() == nil {
		klog.Infof("Found stale VFS cache mount at %s, unmounting", mountPath)
		_ = exec.Command("umount", "-l", mountPath).Run()
	}

	// Close LUKS mapper if it exists
	if _, err := os.Stat(mappedDevice); err == nil {
		klog.Infof("Found stale LUKS mapper %s, closing", VFSCacheMapperName)
		_ = luksManager.CloseLUKS(VFSCacheMapperName)
	}

	// Detach loop device if attached
	if _, err := os.Stat(VFSCacheBackingFile); err == nil {
		if loopDevice, err := findLoopDevice(VFSCacheBackingFile); err == nil && loopDevice != "" {
			klog.Infof("Found stale loop device %s, detaching", loopDevice)
			_ = detachLoopDevice(loopDevice)
		}
	}
}

// Helper functions (these might already exist in syncmanager.go, but included for completeness)

func setupLoopDevice(backingFile string) (string, error) {
	cmd := exec.Command("losetup", "-f", "--show", backingFile)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("losetup failed: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

func findLoopDevice(backingFile string) (string, error) {
	cmd := exec.Command("losetup", "-j", backingFile)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(output))
	if line == "" {
		return "", fmt.Errorf("no loop device found")
	}
	parts := strings.Split(line, ":")
	if len(parts) > 0 {
		return parts[0], nil
	}
	return "", fmt.Errorf("could not parse losetup output")
}

func detachLoopDevice(loopDevice string) error {
	cmd := exec.Command("losetup", "-d", loopDevice)
	return cmd.Run()
}

func formatExt4(device string) error {
	cmd := exec.Command("mkfs.ext4", "-F", device)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func mountFilesystem(device, mountPath string) error {
	// Check if already mounted
	data, _ := os.ReadFile("/proc/mounts")
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == mountPath {
			klog.Infof("Filesystem already mounted at %s", mountPath)
			return nil
		}
	}

	cmd := exec.Command("mount", device, mountPath)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func unmountFilesystem(mountPath string) error {
	cmd := exec.Command("umount", mountPath)
	return cmd.Run()
}