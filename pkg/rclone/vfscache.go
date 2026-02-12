package rclone

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lukscryptwalker-csi/pkg/luks"
	"k8s.io/klog"
)

var (
	// vfsCacheSize stores the configured VFS cache size in bytes
	vfsCacheSize int64 = 20 * 1024 * 1024 * 1024 // 20GB default
	// vfsNameMapMu protects reads/writes to the vfsName map file
	vfsNameMapMu sync.Mutex
)

const vfsNameMapFile = VFSCacheBasePath + "/.vfs-names.json"

const (
	// VFSCacheBasePath is the base path for the encrypted VFS cache
	// Mount directly at rclone's default cache location
	VFSCacheBasePath = "/root/.cache/rclone"
	// VFSCacheMapperName is the LUKS mapper name for VFS cache
	VFSCacheMapperName = "luks-vfs-cache"
	// VFSCacheBackingFile is the backing file for VFS cache LUKS volume
	// Stored inside the host-mounted vfs-cache-dir so it persists across pod restarts,
	// allowing rclone to resume syncing cached data that wasn't uploaded before a crash
	VFSCacheBackingFile = "/var/lib/lukscrypt-vfs-cache/vfs-cache.luks"
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



// GetVFSCacheSize returns the configured VFS cache size in bytes
func GetVFSCacheSize() int64 {
	return vfsCacheSize
}

// LoadVFSNameMap reads the persisted volumeID→vfsName mapping from disk.
// Returns an empty map if the file does not exist or cannot be read.
func LoadVFSNameMap() map[string]string {
	data, err := os.ReadFile(vfsNameMapFile)
	if err != nil {
		if !os.IsNotExist(err) {
			klog.Warningf("Failed to read VFS name map file %s: %v", vfsNameMapFile, err)
		}
		return make(map[string]string)
	}

	m := make(map[string]string)
	if err := json.Unmarshal(data, &m); err != nil {
		klog.Warningf("Failed to parse VFS name map file %s: %v", vfsNameMapFile, err)
		return make(map[string]string)
	}
	return m
}

// writeVFSNameMap writes the volumeID→vfsName mapping to disk.
// Caller must hold vfsNameMapMu.
func writeVFSNameMap(m map[string]string) {
	data, err := json.Marshal(m)
	if err != nil {
		klog.Warningf("Failed to marshal VFS name map: %v", err)
		return
	}
	if err := os.WriteFile(vfsNameMapFile, data, 0600); err != nil {
		klog.Warningf("Failed to write VFS name map file %s: %v", vfsNameMapFile, err)
	}
}

// SaveVFSName persists a volumeID→vfsName mapping to the JSON file.
func SaveVFSName(volumeID, vfsName string) {
	vfsNameMapMu.Lock()
	defer vfsNameMapMu.Unlock()

	m := LoadVFSNameMap()
	m[volumeID] = vfsName
	writeVFSNameMap(m)
	klog.V(4).Infof("Saved VFS name mapping: %s -> %s", volumeID, vfsName)
}

// RemoveVFSName removes a volumeID entry from the persisted mapping.
func RemoveVFSName(volumeID string) {
	vfsNameMapMu.Lock()
	defer vfsNameMapMu.Unlock()

	m := LoadVFSNameMap()
	if _, exists := m[volumeID]; !exists {
		return
	}
	delete(m, volumeID)
	writeVFSNameMap(m)
	klog.V(4).Infof("Removed VFS name mapping for volume %s", volumeID)
}

// CleanupOrphanedVFSCacheDirs removes VFS cache directories for volumes
// that are no longer active. For each entry in the persisted map where the
// volumeID is NOT in activeVolumeIDs, it removes the cache directory and
// the map entry.
func CleanupOrphanedVFSCacheDirs(activeVolumeIDs map[string]bool) {
	vfsNameMapMu.Lock()
	defer vfsNameMapMu.Unlock()

	m := LoadVFSNameMap()
	if len(m) == 0 {
		return
	}

	changed := false
	for volumeID, vfsName := range m {
		if activeVolumeIDs[volumeID] {
			continue
		}

		cacheDir := fmt.Sprintf("%s/vfs/%s", VFSCacheBasePath, vfsName)
		metaDir := fmt.Sprintf("%s/vfsMeta/%s", VFSCacheBasePath, vfsName)
		klog.Infof("Cleaning up orphaned VFS cache for volume %s: %s", volumeID, cacheDir)

		if err := os.RemoveAll(cacheDir); err != nil {
			klog.Warningf("Failed to remove orphaned VFS cache dir %s: %v", cacheDir, err)
		}
		if err := os.RemoveAll(metaDir); err != nil {
			klog.Warningf("Failed to remove orphaned VFS meta dir %s: %v", metaDir, err)
		}

		delete(m, volumeID)
		changed = true
		klog.Infof("Successfully removed orphaned VFS cache for volume %s", volumeID)
	}

	if changed {
		writeVFSNameMap(m)
	}
}
