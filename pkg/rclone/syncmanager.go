package rclone

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lukscryptwalker-csi/pkg/luks"
	"k8s.io/klog"
)

// VFSCacheConfig holds VFS cache configuration options
type VFSCacheConfig struct {
	CacheMode    string // off, minimal, writes, full
	CacheMaxAge  string // e.g., "1h", "24h"
	CacheMaxSize string // e.g., "10G", "100M"
	WriteBack    string // e.g., "5s", "0" for immediate
	CacheDir     string // directory for cache files
}

// DefaultVFSCacheConfig returns sensible defaults for VFS caching
func DefaultVFSCacheConfig() *VFSCacheConfig {
	return &VFSCacheConfig{
		CacheMode:    "full",
		CacheMaxAge:  "1h",
		CacheMaxSize: "10G",
		WriteBack:    "5s",
		CacheDir:     "",
	}
}

// MountManager handles rclone mount operations for encrypted S3 volumes
type MountManager struct {
	s3Config       *S3Config
	cryptConfig    *CryptConfig
	vfsConfig      *VFSCacheConfig
	volumeID       string
	mountPoint     string
	s3BasePath     string
	mounted        bool
	mutex          sync.RWMutex
	luksPassphrase string
	luksManager    *luks.LUKSManager
	cacheBasePath  string // Base path for cache storage (e.g., /var/lib/lukscrypt-cache)
	cacheMounted   bool   // Whether the encrypted cache is mounted
}

// DefaultCacheBasePath is the default base path for encrypted cache storage
const DefaultCacheBasePath = "/var/lib/lukscrypt-cache"

// NewMountManager creates a new rclone mount manager
// s3PathPrefix is optional - if empty, defaults to "volumes/{volumeID}/files"
// If s3PathPrefix is provided, path becomes "{s3PathPrefix}/volumes/{volumeID}/files"
func NewMountManager(s3Config *S3Config, volumeID, mountPoint, luksPassphrase string, vfsConfig *VFSCacheConfig, s3PathPrefix string) (*MountManager, error) {
	if vfsConfig == nil {
		vfsConfig = DefaultVFSCacheConfig()
	}

	// Determine S3 base path - always include volumeID to ensure each volume has its own directory
	var s3BasePath string
	if s3PathPrefix == "" {
		s3BasePath = fmt.Sprintf("volumes/%s/files", volumeID)
	} else {
		s3BasePath = fmt.Sprintf("%s/volumes/%s/files", s3PathPrefix, volumeID)
	}

	// Use s3PathPrefix as salt for password2 if set, otherwise use volumeID
	// This allows users to access data externally using just passphrase + path prefix
	salt := s3PathPrefix
	if salt == "" {
		salt = volumeID
	}
	cryptConfig := DeriveRcloneCryptConfig(luksPassphrase, salt)

	manager := &MountManager{
		s3Config:       s3Config,
		cryptConfig:    cryptConfig,
		vfsConfig:      vfsConfig,
		volumeID:       volumeID,
		mountPoint:     mountPoint,
		s3BasePath:     s3BasePath,
		luksPassphrase: luksPassphrase,
		luksManager:    luks.NewLUKSManager(),
		cacheBasePath:  DefaultCacheBasePath,
	}

	klog.Infof("Created rclone mount manager for volume %s at %s (s3Path: %s)", volumeID, mountPoint, s3BasePath)
	return manager, nil
}

// Mount mounts the encrypted S3 remote at the mount point using librclone
func (mm *MountManager) Mount() error {
	mm.mutex.Lock()
	defer mm.mutex.Unlock()

	if mm.mounted && mm.isMountPoint() {
		klog.Infof("Volume %s already mounted at %s", mm.volumeID, mm.mountPoint)
		return nil
	}

	klog.Infof("Mounting encrypted S3 volume %s at %s", mm.volumeID, mm.mountPoint)

	// Setup encrypted cache first (if caching is enabled)
	encryptedCachePath, err := mm.setupEncryptedCache()
	if err != nil {
		return fmt.Errorf("failed to setup encrypted cache: %w", err)
	}

	// Check for stale mount and try to clean it up
	if mm.isMountPoint() {
		klog.Warningf("Found stale mount at %s, attempting to unmount first", mm.mountPoint)
		// Try to unmount via librclone
		_, _ = RPC("mount/unmount", map[string]interface{}{"mountPoint": mm.mountPoint})
		// Also try fusermount as fallback
		if mm.isMountPoint() {
			klog.Warningf("librclone unmount didn't work, trying fusermount -uz %s", mm.mountPoint)
			// Use lazy unmount to handle busy mounts
			cmd := exec.Command("fusermount", "-uz", mm.mountPoint)
			if err := cmd.Run(); err != nil {
				klog.Warningf("fusermount failed: %v, will try mounting anyway with AllowNonEmpty", err)
			}
		}
	}

	// Ensure mount point exists
	if err := os.MkdirAll(mm.mountPoint, 0755); err != nil {
		_ = mm.teardownEncryptedCache() // Best effort cleanup
		return fmt.Errorf("failed to create mount point: %w", err)
	}

	// Build the crypt remote string for librclone
	cryptRemote, err := BuildCryptRemoteString(mm.s3Config, mm.cryptConfig, mm.s3BasePath)
	if err != nil {
		_ = mm.teardownEncryptedCache() // Best effort cleanup
		return fmt.Errorf("failed to build crypt remote: %w", err)
	}

	// Build mount options
	mountOpt := map[string]interface{}{
		"AllowOther":    true,
		"AllowNonEmpty": true,
		"DirCacheTime":  300000000000, // 5 minutes in nanoseconds
	}

	// Build VFS options, using encrypted cache path if available
	vfsOpt := mm.buildVFSOpt()
	if encryptedCachePath != "" {
		vfsOpt["CacheDir"] = encryptedCachePath
		klog.Infof("Using encrypted cache directory: %s", encryptedCachePath)
	}

	// Call mount/mount RPC
	params := map[string]interface{}{
		"fs":         cryptRemote,
		"mountPoint": mm.mountPoint,
		"mountOpt":   mountOpt,
		"vfsOpt":     vfsOpt,
	}

	klog.V(4).Infof("Calling mount/mount RPC for volume %s", mm.volumeID)

	_, err = RPC("mount/mount", params)
	if err != nil {
		_ = mm.teardownEncryptedCache() // Best effort cleanup
		return fmt.Errorf("failed to mount: %w", err)
	}

	mm.mounted = true
	klog.Infof("Successfully mounted encrypted S3 volume %s at %s", mm.volumeID, mm.mountPoint)
	return nil
}

// buildVFSOpt builds VFS options for the mount
func (mm *MountManager) buildVFSOpt() map[string]interface{} {
	vfsOpt := map[string]interface{}{
		"ReadChunkSize":      33554432, // 32M in bytes
		"ReadChunkSizeLimit": -1,       // off
		"BufferSize":         33554432, // 32M in bytes
	}

	// Cache mode
	if mm.vfsConfig.CacheMode != "" {
		// Map string to CacheMode value
		switch mm.vfsConfig.CacheMode {
		case "off":
			vfsOpt["CacheMode"] = 0
		case "minimal":
			vfsOpt["CacheMode"] = 1
		case "writes":
			vfsOpt["CacheMode"] = 2
		case "full":
			vfsOpt["CacheMode"] = 3
		}
	}

	// Cache max age (parse duration string to nanoseconds)
	if mm.vfsConfig.CacheMaxAge != "" {
		if ns, err := parseDurationToNs(mm.vfsConfig.CacheMaxAge); err == nil {
			vfsOpt["CacheMaxAge"] = ns
		}
	}

	// Cache max size (parse size string to bytes)
	if mm.vfsConfig.CacheMaxSize != "" {
		if bytes, err := parseSizeToBytes(mm.vfsConfig.CacheMaxSize); err == nil {
			vfsOpt["CacheMaxSize"] = bytes
		}
	}

	// Write back delay
	if mm.vfsConfig.WriteBack != "" {
		if ns, err := parseDurationToNs(mm.vfsConfig.WriteBack); err == nil {
			vfsOpt["WriteBack"] = ns
		}
	}

	// Cache directory
	if mm.vfsConfig.CacheDir != "" {
		vfsOpt["CacheDir"] = mm.vfsConfig.CacheDir
	}

	return vfsOpt
}

// Unmount unmounts the S3 volume using librclone
func (mm *MountManager) Unmount() error {
	mm.mutex.Lock()
	defer mm.mutex.Unlock()

	if !mm.mounted && !mm.isMountPoint() && !mm.cacheMounted {
		klog.Infof("Volume %s not mounted, skipping unmount", mm.volumeID)
		return nil
	}

	klog.Infof("Unmounting encrypted S3 volume %s from %s", mm.volumeID, mm.mountPoint)

	// Call mount/unmount RPC
	params := map[string]interface{}{
		"mountPoint": mm.mountPoint,
	}

	_, err := RPC("mount/unmount", params)
	if err != nil {
		klog.Warningf("mount/unmount RPC failed: %v", err)
		// Try unmountall as fallback
		_, err = RPC("mount/unmountall", map[string]interface{}{})
		if err != nil {
			klog.Warningf("Failed to unmount rclone: %v", err)
		}
	}

	mm.mounted = false

	// Teardown encrypted cache after unmounting rclone
	if err := mm.teardownEncryptedCache(); err != nil {
		klog.Warningf("Failed to teardown encrypted cache: %v", err)
	}

	klog.Infof("Successfully unmounted encrypted S3 volume %s", mm.volumeID)
	return nil
}

// isMountPoint checks if the path is a mount point
func (mm *MountManager) isMountPoint() bool {
	// Check /proc/mounts for FUSE mounts
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		klog.V(5).Infof("Failed to read /proc/mounts: %v", err)
		return false
	}

	// Look for rclone or fuse mount at our path
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == mm.mountPoint {
			klog.V(5).Infof("Found mount at %s: %s", mm.mountPoint, line)
			return true
		}
	}

	return false
}

// IsMounted returns whether the volume is currently mounted
func (mm *MountManager) IsMounted() bool {
	mm.mutex.RLock()
	defer mm.mutex.RUnlock()
	return mm.mounted && mm.isMountPoint()
}

// getCacheBackingFile returns the path to the LUKS backing file for cache
func (mm *MountManager) getCacheBackingFile() string {
	return filepath.Join(mm.cacheBasePath, "backing", fmt.Sprintf("%s.luks", mm.volumeID))
}

// getCacheMountPath returns the mount path for the encrypted cache
func (mm *MountManager) getCacheMountPath() string {
	return filepath.Join(mm.cacheBasePath, "mounts", mm.volumeID)
}

// getCacheMapperName returns the LUKS mapper name for the cache
func (mm *MountManager) getCacheMapperName() string {
	return fmt.Sprintf("luks-cache-%s", mm.volumeID)
}

// setupEncryptedCache creates and mounts a LUKS-encrypted cache directory
func (mm *MountManager) setupEncryptedCache() (string, error) {
	// If cache mode is off, no need for encrypted cache
	if mm.vfsConfig.CacheMode == "off" {
		klog.Infof("VFS cache mode is off, skipping encrypted cache setup for volume %s", mm.volumeID)
		return "", nil
	}

	backingFile := mm.getCacheBackingFile()
	mountPath := mm.getCacheMountPath()
	mapperName := mm.getCacheMapperName()

	klog.Infof("Setting up encrypted cache for volume %s: backing=%s, mount=%s", mm.volumeID, backingFile, mountPath)

	// Create directories
	if err := os.MkdirAll(filepath.Dir(backingFile), 0700); err != nil {
		return "", fmt.Errorf("failed to create cache backing directory: %w", err)
	}
	if err := os.MkdirAll(mountPath, 0700); err != nil {
		return "", fmt.Errorf("failed to create cache mount directory: %w", err)
	}

	// Parse cache size for backing file (default to 1GB if not specified or on error)
	cacheSize := int64(1 * 1024 * 1024 * 1024) // 1GB default
	if mm.vfsConfig.CacheMaxSize != "" {
		if parsed, err := parseSizeToBytes(mm.vfsConfig.CacheMaxSize); err == nil && parsed > 0 {
			cacheSize = parsed
		}
	}

	// Create backing file if it doesn't exist
	if _, err := os.Stat(backingFile); os.IsNotExist(err) {
		klog.Infof("Creating cache backing file %s with size %d bytes", backingFile, cacheSize)

		// Create sparse file
		f, err := os.Create(backingFile)
		if err != nil {
			return "", fmt.Errorf("failed to create cache backing file: %w", err)
		}
		if err := f.Truncate(cacheSize); err != nil {
			_ = f.Close()
			_ = os.Remove(backingFile)
			return "", fmt.Errorf("failed to set cache backing file size: %w", err)
		}
		_ = f.Close()

		// Setup loop device and format with LUKS
		loopDevice, err := mm.setupLoopDevice(backingFile)
		if err != nil {
			_ = os.Remove(backingFile)
			return "", fmt.Errorf("failed to setup loop device: %w", err)
		}

		// Format with LUKS
		if err := mm.luksManager.FormatAndOpenLUKS(loopDevice, mapperName, mm.luksPassphrase); err != nil {
			_ = mm.detachLoopDevice(loopDevice) // Best effort cleanup
			_ = os.Remove(backingFile)
			return "", fmt.Errorf("failed to format LUKS cache: %w", err)
		}

		// Format the mapped device with ext4
		mappedDevice := mm.luksManager.GetMappedDevicePath(mapperName)
		if err := mm.formatExt4(mappedDevice); err != nil {
			_ = mm.luksManager.CloseLUKS(mapperName) // Best effort cleanup
			_ = mm.detachLoopDevice(loopDevice)
			_ = os.Remove(backingFile)
			return "", fmt.Errorf("failed to format cache filesystem: %w", err)
		}
	} else {
		// Backing file exists, just open it
		loopDevice, err := mm.setupLoopDevice(backingFile)
		if err != nil {
			return "", fmt.Errorf("failed to setup loop device: %w", err)
		}

		if err := mm.luksManager.OpenLUKS(loopDevice, mapperName, mm.luksPassphrase); err != nil {
			_ = mm.detachLoopDevice(loopDevice) // Best effort cleanup
			return "", fmt.Errorf("failed to open LUKS cache: %w", err)
		}
	}

	// Mount the encrypted cache
	mappedDevice := mm.luksManager.GetMappedDevicePath(mapperName)
	if err := mm.mountFilesystem(mappedDevice, mountPath); err != nil {
		_ = mm.luksManager.CloseLUKS(mapperName) // Best effort cleanup
		return "", fmt.Errorf("failed to mount cache filesystem: %w", err)
	}

	mm.cacheMounted = true
	klog.Infof("Successfully set up encrypted cache at %s for volume %s", mountPath, mm.volumeID)
	return mountPath, nil
}

// teardownEncryptedCache unmounts and closes the encrypted cache
func (mm *MountManager) teardownEncryptedCache() error {
	if !mm.cacheMounted {
		return nil
	}

	mountPath := mm.getCacheMountPath()
	mapperName := mm.getCacheMapperName()
	backingFile := mm.getCacheBackingFile()

	klog.Infof("Tearing down encrypted cache for volume %s", mm.volumeID)

	// Unmount the filesystem
	if err := mm.unmountFilesystem(mountPath); err != nil {
		klog.Warningf("Failed to unmount cache filesystem: %v", err)
	}

	// Close LUKS
	if err := mm.luksManager.CloseLUKS(mapperName); err != nil {
		klog.Warningf("Failed to close LUKS cache: %v", err)
	}

	// Find and detach loop device
	loopDevice, err := mm.findLoopDevice(backingFile)
	if err == nil && loopDevice != "" {
		_ = mm.detachLoopDevice(loopDevice) // Best effort cleanup
	}

	mm.cacheMounted = false
	klog.Infof("Successfully tore down encrypted cache for volume %s", mm.volumeID)
	return nil
}

// setupLoopDevice creates a loop device for the backing file
func (mm *MountManager) setupLoopDevice(backingFile string) (string, error) {
	// First check if already attached
	existing, err := mm.findLoopDevice(backingFile)
	if err == nil && existing != "" {
		klog.Infof("Loop device %s already attached for %s", existing, backingFile)
		return existing, nil
	}

	cmd := exec.Command("losetup", "-f", "--show", backingFile)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("losetup failed: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// findLoopDevice finds the loop device for a backing file
func (mm *MountManager) findLoopDevice(backingFile string) (string, error) {
	cmd := exec.Command("losetup", "-j", backingFile)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(output))
	if line == "" {
		return "", fmt.Errorf("no loop device found")
	}
	// Output format: /dev/loop0: [64769]:123456 (/path/to/file)
	parts := strings.Split(line, ":")
	if len(parts) > 0 {
		return parts[0], nil
	}
	return "", fmt.Errorf("could not parse losetup output")
}

// detachLoopDevice detaches a loop device
func (mm *MountManager) detachLoopDevice(loopDevice string) error {
	cmd := exec.Command("losetup", "-d", loopDevice)
	return cmd.Run()
}

// formatExt4 formats a device with ext4
func (mm *MountManager) formatExt4(device string) error {
	cmd := exec.Command("mkfs.ext4", "-F", device)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// mountFilesystem mounts a device at a mount point
func (mm *MountManager) mountFilesystem(device, mountPath string) error {
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

// unmountFilesystem unmounts a mount point
func (mm *MountManager) unmountFilesystem(mountPath string) error {
	cmd := exec.Command("umount", mountPath)
	return cmd.Run()
}

// ForceSync flushes any cached writes
func (mm *MountManager) ForceSync() error {
	// With rclone mount via librclone, we can use vfs/refresh to ensure cache is flushed
	params := map[string]interface{}{
		"fs":        mm.mountPoint,
		"recursive": true,
	}

	_, err := RPC("vfs/refresh", params)
	if err != nil {
		klog.V(4).Infof("vfs/refresh returned error (may be normal): %v", err)
	}
	return nil
}

// parseDurationToNs parses a duration string like "1h" or "5s" to nanoseconds
func parseDurationToNs(s string) (int64, error) {
	// Simple parser for common duration formats
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid duration: %s", s)
	}

	unit := s[len(s)-1]
	value := s[:len(s)-1]

	var num int64
	_, err := fmt.Sscanf(value, "%d", &num)
	if err != nil {
		return 0, err
	}

	switch unit {
	case 's':
		return num * 1e9, nil
	case 'm':
		return num * 60 * 1e9, nil
	case 'h':
		return num * 3600 * 1e9, nil
	case 'd':
		return num * 86400 * 1e9, nil
	default:
		return 0, fmt.Errorf("unknown duration unit: %c", unit)
	}
}

// parseSizeToBytes parses a size string like "10G" or "100M" to bytes
func parseSizeToBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid size: %s", s)
	}

	unit := s[len(s)-1]
	value := s[:len(s)-1]

	var num int64
	_, err := fmt.Sscanf(value, "%d", &num)
	if err != nil {
		return 0, err
	}

	switch unit {
	case 'K', 'k':
		return num * 1024, nil
	case 'M', 'm':
		return num * 1024 * 1024, nil
	case 'G', 'g':
		return num * 1024 * 1024 * 1024, nil
	case 'T', 't':
		return num * 1024 * 1024 * 1024 * 1024, nil
	default:
		return 0, fmt.Errorf("unknown size unit: %c", unit)
	}
}

// ============================================================================
// Legacy SyncManager interface for backward compatibility
// ============================================================================

// SyncManager is an alias for MountManager
type SyncManager = MountManager

// NewSyncManager creates a new mount manager (backward compatible name)
func NewSyncManager(s3Config *S3Config, volumeID, mountPoint, luksPassphrase string) (*MountManager, error) {
	return NewMountManager(s3Config, volumeID, mountPoint, luksPassphrase, nil, "")
}

// SyncToS3 triggers a cache flush
func (mm *MountManager) SyncToS3() error {
	return mm.ForceSync()
}

// SyncFromS3 is a no-op for mount mode
func (mm *MountManager) SyncFromS3() error {
	return nil
}

// SyncToRemote is an alias for ForceSync
func (mm *MountManager) SyncToRemote() error {
	return mm.ForceSync()
}

// SyncFromRemote is a no-op for mount mode
func (mm *MountManager) SyncFromRemote() error {
	return nil
}

// SyncSingleFile is a no-op for mount mode
func (mm *MountManager) SyncSingleFile(relativePath string) error {
	return nil
}

// SyncDeletedFile is a no-op for mount mode
func (mm *MountManager) SyncDeletedFile(relativePath string) error {
	return nil
}
