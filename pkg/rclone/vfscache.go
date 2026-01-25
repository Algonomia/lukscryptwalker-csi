package rclone

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lukscryptwalker-csi/pkg/luks"
	"k8s.io/klog"
)

var (
	// vfsCacheCleanupStop is used to signal the cleanup goroutine to stop
	vfsCacheCleanupStop chan struct{}
	vfsCacheCleanupMu   sync.Mutex
	// vfsCacheSize stores the configured VFS cache size in bytes
	vfsCacheSize int64 = 20 * 1024 * 1024 * 1024 // 20GB default
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

	// Store the cache size globally for later use
	vfsCacheSize = cacheSize

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

// StartVFSCacheCleanup starts the background VFS cache cleanup goroutine
// This should be called after SetupVFSCache succeeds
func StartVFSCacheCleanup(cleanupInterval time.Duration, diskUsageThreshold float64) {
	vfsCacheCleanupMu.Lock()
	defer vfsCacheCleanupMu.Unlock()

	if vfsCacheCleanupStop != nil {
		klog.Infof("VFS cache cleanup already running")
		return
	}

	vfsCacheCleanupStop = make(chan struct{})

	go vfsCacheCleanupLoop(cleanupInterval, diskUsageThreshold, vfsCacheCleanupStop)
	klog.Infof("Started VFS cache cleanup goroutine with interval=%v, diskUsageThreshold=%.0f%%",
		cleanupInterval, diskUsageThreshold*100)
}

// StopVFSCacheCleanup stops the background VFS cache cleanup goroutine
func StopVFSCacheCleanup() {
	vfsCacheCleanupMu.Lock()
	defer vfsCacheCleanupMu.Unlock()

	if vfsCacheCleanupStop != nil {
		close(vfsCacheCleanupStop)
		vfsCacheCleanupStop = nil
		klog.Infof("Stopped VFS cache cleanup goroutine")
	}
}

// vfsCacheCleanupLoop is the main cleanup loop that runs in the background
func vfsCacheCleanupLoop(interval time.Duration, diskUsageThreshold float64, stop chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run cleanup immediately on start
	runVFSCacheCleanup(diskUsageThreshold)

	for {
		select {
		case <-ticker.C:
			runVFSCacheCleanup(diskUsageThreshold)
		case <-stop:
			klog.Infof("VFS cache cleanup goroutine stopping")
			return
		}
	}
}

// runVFSCacheCleanup performs a single cleanup pass
func runVFSCacheCleanup(diskUsageThreshold float64) {
	mountPath := VFSCacheBasePath

	// Check if the VFS cache is mounted
	if !isVFSCacheMounted(mountPath) {
		klog.V(5).Infof("VFS cache not mounted, skipping cleanup")
		return
	}

	// Check disk usage and perform aggressive cleanup if needed
	usagePercent, err := getVFSCacheDiskUsage(mountPath)
	if err != nil {
		klog.V(4).Infof("Failed to get VFS cache disk usage: %v", err)
	} else {
		klog.V(5).Infof("VFS cache disk usage: %.1f%%", usagePercent*100)

		if usagePercent >= diskUsageThreshold {
			klog.Warningf("VFS cache disk usage (%.1f%%) exceeds threshold (%.0f%%), performing aggressive cleanup",
				usagePercent*100, diskUsageThreshold*100)
			aggressiveVFSCacheCleanup(mountPath)
		}
	}

	// Always clean up empty directories
	removedDirs := cleanupEmptyDirectories(mountPath)
	if removedDirs > 0 {
		klog.Infof("Cleaned up %d empty directories from VFS cache", removedDirs)
	}
}

// getVFSCacheDiskUsage returns the disk usage percentage (0.0-1.0) of the VFS cache mount
func getVFSCacheDiskUsage(mountPath string) (float64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(mountPath, &stat); err != nil {
		return 0, fmt.Errorf("statfs failed: %w", err)
	}

	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bfree * uint64(stat.Bsize)
	used := total - free

	if total == 0 {
		return 0, fmt.Errorf("total size is zero")
	}

	return float64(used) / float64(total), nil
}

// cleanupEmptyDirectories removes empty directories from the VFS cache
// Returns the number of directories removed
func cleanupEmptyDirectories(basePath string) int {
	removedCount := 0

	// Walk the directory tree from bottom up to remove empty directories
	// We need multiple passes since removing a directory may make its parent empty
	for {
		removed := 0
		err := filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // Skip paths we can't access
			}

			// Skip the base path itself
			if path == basePath {
				return nil
			}

			// Only process directories
			if !info.IsDir() {
				return nil
			}

			// Check if directory is empty
			entries, err := os.ReadDir(path)
			if err != nil {
				return nil // Skip if we can't read
			}

			if len(entries) == 0 {
				if err := os.Remove(path); err != nil {
					klog.V(5).Infof("Failed to remove empty directory %s: %v", path, err)
				} else {
					klog.V(4).Infof("Removed empty VFS cache directory: %s", path)
					removed++
				}
			}

			return nil
		})

		if err != nil {
			klog.V(4).Infof("Error walking VFS cache directory: %v", err)
			break
		}

		if removed == 0 {
			break // No more empty directories
		}
		removedCount += removed
	}

	return removedCount
}

// aggressiveVFSCacheCleanup performs aggressive cleanup when disk usage is high
// This removes old cached files to free up space
func aggressiveVFSCacheCleanup(basePath string) {
	klog.Infof("Starting aggressive VFS cache cleanup at %s", basePath)

	// Find and remove old cache files (files not accessed recently)
	// rclone stores cache in vfs/<remote>/... structure
	vfsDir := filepath.Join(basePath, "vfs")
	if _, err := os.Stat(vfsDir); os.IsNotExist(err) {
		klog.V(4).Infof("VFS directory %s does not exist, skipping aggressive cleanup", vfsDir)
		return
	}

	// Calculate cutoff time - remove files older than 30 minutes when under pressure
	cutoffTime := time.Now().Add(-30 * time.Minute)
	removedFiles := 0
	freedBytes := int64(0)

	err := filepath.Walk(vfsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip inaccessible paths
		}

		// Skip directories
		if info.IsDir() {
			return nil
		}

		// Check if file is old enough to remove
		if info.ModTime().Before(cutoffTime) {
			size := info.Size()
			if err := os.Remove(path); err != nil {
				klog.V(5).Infof("Failed to remove old cache file %s: %v", path, err)
			} else {
				removedFiles++
				freedBytes += size
				klog.V(4).Infof("Removed old cache file: %s (age: %v)", path, time.Since(info.ModTime()))
			}
		}

		return nil
	})

	if err != nil {
		klog.Warningf("Error during aggressive VFS cache cleanup: %v", err)
	}

	if removedFiles > 0 {
		klog.Infof("Aggressive cleanup removed %d files, freed %.2f MB",
			removedFiles, float64(freedBytes)/(1024*1024))
	}

	// Also clean up empty directories after removing files
	cleanupEmptyDirectories(basePath)
}

// GetVFSCacheSize returns the configured VFS cache size in bytes
func GetVFSCacheSize() int64 {
	return vfsCacheSize
}