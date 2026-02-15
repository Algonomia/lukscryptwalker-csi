package rclone

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"k8s.io/klog"
)

// VFSCacheConfig holds VFS cache configuration options
type VFSCacheConfig struct {
	CacheMode         string // off, minimal, writes, full
	CacheMaxAge       string // e.g., "1h", "24h"
	CacheMaxSize      string // e.g., "10G", "100M"
	CachePollInterval string // e.g., "1m", "5m" - how often to poll for stale cache entries
	WriteBack         string // e.g., "5s", "0" for immediate
}

// DefaultVFSCacheConfig returns sensible defaults for VFS caching
func DefaultVFSCacheConfig() *VFSCacheConfig {
	return &VFSCacheConfig{
		CacheMode:         "full",
		CacheMaxAge:       "4h",  // 4 hours to handle long-running operations
		CacheMaxSize:      "5G", // 5GB to handle large files
		CachePollInterval: "1m",  // Poll every minute for stale cache entries
		WriteBack:         "3s",  // Start uploads quickly to reduce cache pressure
	}
}

// MountManager handles rclone mount operations for encrypted S3 volumes
type MountManager struct {
	s3Config        *S3Config
	cryptConfig     *CryptConfig
	vfsConfig       *VFSCacheConfig
	volumeID        string
	mountPoint      string
	s3BasePath      string
	mounted         bool
	vfsName         string // Deterministic VFS name derived from volumeID
	s3ConfigName    string // Named rclone config for S3 backend
	cryptConfigName string // Named rclone config for crypt layer
}

// NewMountManager creates a new rclone mount manager
// s3PathPrefix is optional - if empty, defaults to "volumes/{volumeID}/files"
// If s3PathPrefix is provided, path becomes "{s3PathPrefix}/volumes/{volumeID}/files"
func NewMountManager(s3Config *S3Config, volumeID, mountPoint string, vfsConfig *VFSCacheConfig, s3PathPrefix string, luksPassphrase string) (*MountManager, error) {
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
		s3Config:        s3Config,
		cryptConfig:     cryptConfig,
		vfsConfig:       vfsConfig,
		volumeID:        volumeID,
		mountPoint:      mountPoint,
		s3BasePath:      s3BasePath,
		vfsName:         volumeID,
		s3ConfigName:    volumeID + "-s3",
		cryptConfigName: volumeID,
	}

	klog.Infof("Created rclone mount manager for volume %s at %s (s3Path: %s)", volumeID, mountPoint, s3BasePath)
	return manager, nil
}

// Mount mounts the encrypted S3 remote at the mount point using librclone
func (mm *MountManager) Mount() error {

	if mm.mounted && mm.isMountPoint() {
		klog.Infof("Volume %s already mounted at %s", mm.volumeID, mm.mountPoint)
		return nil
	}

	klog.Infof("Mounting encrypted S3 volume %s at %s", mm.volumeID, mm.mountPoint)

	// Check for stale mount and try to clean it up
	if mm.isMountPoint() {
		klog.Warningf("Found stale mount at %s, attempting to unmount first", mm.mountPoint)
		if err := mm.Unmount(); err != nil {
			klog.Warningf("Unmount failed: %v, will try mounting anyway with AllowNonEmpty", err)
		}
	}

	// Detect stale cache from a previous ungraceful shutdown.
	// After a crash, vfsMeta/ may reference data files in vfs/ that were lost
	// (incomplete writes). On clean shutdown, cleanupVFSCacheDir() removes both.
	hadStaleCache := mm.hasStaleVFSCache()

	// Ensure mount point exists
	if err := os.MkdirAll(mm.mountPoint, 0755); err != nil {
		return fmt.Errorf("failed to create mount point: %w", err)
	}

	// Create named rclone configs (deterministic names for stable VFS cache dirs)
	if err := CreateNamedS3Config(mm.s3ConfigName, mm.s3Config); err != nil {
		return fmt.Errorf("failed to create S3 config: %w", err)
	}

	s3RemotePath := fmt.Sprintf("%s:%s/%s", mm.s3ConfigName, mm.s3Config.Bucket, mm.s3BasePath)
	if err := CreateNamedCryptConfig(mm.cryptConfigName, s3RemotePath, mm.cryptConfig); err != nil {
		DeleteNamedConfigs(mm.s3ConfigName)
		return fmt.Errorf("failed to create crypt config: %w", err)
	}

	// Build mount options
	mountOpt := map[string]interface{}{
		"AllowOther":    true,
		"AllowNonEmpty": true,
		"DirCacheTime":  300000000000, // 5 minutes in nanoseconds
		"AttrTimeout":   300000000000, // 5 minutes in nanoseconds - caches file attributes (stat) in the kernel FUSE layer
	}

	// Build VFS options
	vfsOpt := mm.buildVFSOpt()

	// Call mount/mount RPC using the named crypt remote
	params := map[string]interface{}{
		"fs":         mm.cryptConfigName + ":",
		"mountPoint": mm.mountPoint,
		"mountOpt":   mountOpt,
		"vfsOpt":     vfsOpt,
	}

	klog.Infof("Calling mount/mount RPC for volume %s", mm.volumeID)

	_, err := RPC("mount/mount", params)
	if err != nil {
		DeleteNamedConfigs(mm.cryptConfigName, mm.s3ConfigName)
		return fmt.Errorf("failed to mount: %w", err)
	}

	// Persist the vfsName mapping for orphan cleanup across restarts
	SaveVFSName(mm.volumeID, mm.vfsName)

	mm.mounted = true
	klog.Infof("Successfully mounted encrypted S3 volume %s at %s", mm.volumeID, mm.mountPoint)

	// If stale cache was detected (ungraceful shutdown), refresh the VFS to force
	// rclone to reconcile its cache state against S3. Without this, stale metadata
	// in vfsMeta/ referencing lost data files causes "detected external removal of
	// cache file" errors and rclone fails to re-download from S3.
	if hadStaleCache {
		klog.Infof("Stale VFS cache detected for volume %s, triggering VFS refresh to reconcile with S3", mm.volumeID)
		mm.refreshVFS()
	}

	return nil
}

// buildVFSOpt builds VFS options for the mount
func (mm *MountManager) buildVFSOpt() map[string]interface{} {
	vfsOpt := map[string]interface{}{
		"ReadChunkSize":      33554432, // 32M in bytes
		"ReadChunkSizeLimit": -1,       // off
		"BufferSize":         33554432, // 32M in bytes
		"Links":              true,     // Enable symlink support
	}

	// Set VFS disk space total size to match the LUKS VFS cache volume size
	// This helps rclone manage disk space within the allocated encrypted volume
	vfsOpt["DiskSpaceTotalSize"] = GetVFSCacheSize()

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

	// Cache poll interval - how often to check for stale cache entries
	if mm.vfsConfig.CachePollInterval != "" {
		if ns, err := parseDurationToNs(mm.vfsConfig.CachePollInterval); err == nil {
			vfsOpt["CachePollInterval"] = ns
		}
	}

	return vfsOpt
}

// Unmount unmounts the S3 volume using librclone
func (mm *MountManager) Unmount() error {

	if !mm.mounted && !mm.isMountPoint() {
		klog.Infof("Volume %s not mounted, skipping unmount", mm.volumeID)
		return nil
	}

	klog.Infof("Unmounting encrypted S3 volume %s from %s", mm.volumeID, mm.mountPoint)

	// CRITICAL: Wait for pending uploads to complete before unmounting
	// This prevents data loss when there are cached writes not yet uploaded to S3
	mm.waitForPendingUploads()

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

	// Clean up named rclone configs
	DeleteNamedConfigs(mm.cryptConfigName, mm.s3ConfigName)

	// Clean up the on-disk VFS cache directory after unmount
	mm.cleanupVFSCacheDir()

	mm.mounted = false

	klog.Infof("Successfully unmounted encrypted S3 volume %s", mm.volumeID)
	return nil
}

// waitForPendingUploads waits for any pending VFS cache uploads to complete
// This is critical to prevent data loss on unmount
func (mm *MountManager) waitForPendingUploads() {
	klog.Infof("Waiting for pending uploads to complete for volume %s", mm.volumeID)

	// First, wait for the write-back duration to ensure cached writes are flushed
	// The write-back delay means files may not start uploading until this time elapses
	writeBackWait := 5 // Default 5 seconds
	if mm.vfsConfig.WriteBack != "" {
		if ns, err := parseDurationToNs(mm.vfsConfig.WriteBack); err == nil {
			writeBackWait = int(ns / 1e9) // Convert nanoseconds to seconds
		}
	}
	// Add a small buffer to ensure write-back has completed
	writeBackWait += 2
	klog.Infof("Waiting %d seconds for write-back to complete for volume %s", writeBackWait, mm.volumeID)
	sleepCmd := exec.Command("sleep", fmt.Sprintf("%d", writeBackWait))
	_ = sleepCmd.Run()

	maxWaitTime := 1800 // 30 minutes max wait
	pollInterval := 2  // Check every 2 seconds

	for i := 0; i < maxWaitTime/pollInterval; i++ {
		// Check core/stats for active transfers
		result, err := RPC("core/stats", map[string]interface{}{})
		if err != nil {
			klog.Warningf("Failed to get transfer stats: %v", err)
			break
		}

		// Parse the stats to check for active transfers
		if result != nil && result.Output != nil {
			transfers := int64(0)
			if t, ok := result.Output["transferring"]; ok {
				if tList, ok := t.([]interface{}); ok {
					transfers = int64(len(tList))
				}
			}

			// Also check checks (which includes uploads)
			checks := int64(0)
			if c, ok := result.Output["checking"]; ok {
				if cList, ok := c.([]interface{}); ok {
					checks = int64(len(cList))
				}
			}

			if transfers == 0 && checks == 0 {
				klog.Infof("No pending transfers for volume %s", mm.volumeID)
				break
			}

			klog.Infof("Waiting for %d transfers and %d checks to complete for volume %s", transfers, checks, mm.volumeID)
		}

		// Wait before checking again
		sleepCmd := exec.Command("sleep", fmt.Sprintf("%d", pollInterval))
		_ = sleepCmd.Run()
	}

	klog.Infof("Finished waiting for pending uploads for volume %s", mm.volumeID)
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

// hasStaleVFSCache checks if VFS cache directories exist from a previous mount
// session that wasn't cleanly unmounted (e.g. after a crash or OOM kill).
func (mm *MountManager) hasStaleVFSCache() bool {
	if mm.vfsName == "" {
		return false
	}
	cacheDir := fmt.Sprintf("%s/vfs/%s", VFSCacheBasePath, mm.vfsName)
	metaDir := fmt.Sprintf("%s/vfsMeta/%s", VFSCacheBasePath, mm.vfsName)
	_, err1 := os.Stat(cacheDir)
	_, err2 := os.Stat(metaDir)
	return err1 == nil || err2 == nil
}

// refreshVFS forces rclone to reconcile its VFS state against the S3 remote.
// This clears stale in-memory items and re-reads directory listings from S3,
// so that missing cache files trigger re-downloads instead of errors.
func (mm *MountManager) refreshVFS() {
	fs := mm.cryptConfigName + ":"

	// Forget in-memory directory cache so rclone drops any stale Items
	if _, err := RPC("vfs/forget", map[string]interface{}{"fs": fs}); err != nil {
		klog.Warningf("vfs/forget failed for volume %s: %v", mm.volumeID, err)
	}

	// Refresh directory listings from S3 to rebuild clean state
	refreshParams := map[string]interface{}{
		"fs":        fs,
		"dir":       "",
		"recursive": "true",
	}
	if _, err := RPC("vfs/refresh", refreshParams); err != nil {
		klog.Warningf("vfs/refresh failed for volume %s: %v", mm.volumeID, err)
	}
}

// cleanupVFSCacheDir updates the VFS name mapping after unmount.
// We intentionally do NOT delete rclone's cache directories (vfs/ and vfsMeta/).
// Rclone manages its own cache via CacheMaxAge, CacheMaxSize, and CachePollInterval.
// Externally removing cache files causes "detected external removal of cache file"
// errors. Leaving the cache intact also lets rclone reuse it across remounts.
func (mm *MountManager) cleanupVFSCacheDir() {
	if mm.vfsName == "" {
		return
	}
	RemoveVFSName(mm.volumeID)
}

// IsMounted returns whether the volume is currently mounted
func (mm *MountManager) IsMounted() bool {
	return mm.mounted && mm.isMountPoint()
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

