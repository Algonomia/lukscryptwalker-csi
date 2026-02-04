package rclone

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"k8s.io/klog"
)

var (
	// vfsMountMu protects vfs/list operations during mount to correctly identify new VFS entries
	vfsMountMu sync.Mutex
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
	s3Config    *S3Config
	cryptConfig *CryptConfig
	vfsConfig   *VFSCacheConfig
	volumeID    string
	mountPoint  string
	s3BasePath  string
	mounted     bool
	vfsName     string // VFS name from vfs/list, used for vfs/forget on unmount
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
		s3Config:       s3Config,
		cryptConfig:    cryptConfig,
		vfsConfig:      vfsConfig,
		volumeID:       volumeID,
		mountPoint:     mountPoint,
		s3BasePath:     s3BasePath,
	}

	klog.Infof("Created rclone mount manager for volume %s at %s (s3Path: %s)", volumeID, mountPoint, s3BasePath)
	return manager, nil
}

// getVFSNames returns a set of VFS names from vfs/list RPC
func getVFSNames() (map[string]bool, error) {
	result, err := RPC("vfs/list", map[string]interface{}{})
	if err != nil {
		return nil, fmt.Errorf("vfs/list failed: %w", err)
	}

	names := make(map[string]bool)
	if result != nil && result.Output != nil {
		if vfses, ok := result.Output["vfses"].([]interface{}); ok {
			for _, v := range vfses {
				if vfs, ok := v.(map[string]interface{}); ok {
					if name, ok := vfs["Name"].(string); ok && name != "" {
						names[name] = true
					}
				}
			}
		}
	}
	return names, nil
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

	// Ensure mount point exists
	if err := os.MkdirAll(mm.mountPoint, 0755); err != nil {
		return fmt.Errorf("failed to create mount point: %w", err)
	}

	// Build the crypt remote string for librclone
	cryptRemote, err := BuildCryptRemoteString(mm.s3Config, mm.cryptConfig, mm.s3BasePath)
	if err != nil {
		return fmt.Errorf("failed to build crypt remote: %w", err)
	}

	// Build mount options
	mountOpt := map[string]interface{}{
		"AllowOther":    true,
		"AllowNonEmpty": true,
		"DirCacheTime":  300000000000, // 5 minutes in nanoseconds
	}

	// Build VFS options
	vfsOpt := mm.buildVFSOpt()

	// Call mount/mount RPC
	params := map[string]interface{}{
		"fs":         cryptRemote,
		"mountPoint": mm.mountPoint,
		"mountOpt":   mountOpt,
		"vfsOpt":     vfsOpt,
	}

	// Lock to safely identify the new VFS entry by comparing before/after
	vfsMountMu.Lock()
	defer vfsMountMu.Unlock()

	// Get VFS names before mounting
	vfsNamesBefore, err := getVFSNames()
	if err != nil {
		klog.Warningf("Failed to get VFS names before mount: %v", err)
		vfsNamesBefore = make(map[string]bool)
	}

	klog.Infof("Calling mount/mount RPC for volume %s", mm.volumeID)

	_, err = RPC("mount/mount", params)
	if err != nil {
		return fmt.Errorf("failed to mount: %w", err)
	}

	// Get VFS names after mounting and find the new entry
	vfsNamesAfter, err := getVFSNames()
	if err != nil {
		klog.Warningf("Failed to get VFS names after mount: %v", err)
	} else {
		// Find the new VFS name (present in after but not in before)
		for name := range vfsNamesAfter {
			if !vfsNamesBefore[name] {
				// The vfsName from vfs/list has a trailing colon (e.g., ":crypt{5NTQG}:")
				// but rclone creates cache directories without it, so strip it here
				mm.vfsName = strings.TrimSuffix(name, ":")
				klog.Infof("Identified VFS name for volume %s: %s", mm.volumeID, mm.vfsName)
				break
			}
		}
		if mm.vfsName == "" {
			klog.Warningf("Could not identify VFS name for volume %s", mm.volumeID)
		}
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

// cleanupVFSCacheDir removes the on-disk VFS cache directory after unmount
func (mm *MountManager) cleanupVFSCacheDir() {
	if mm.vfsName == "" {
		klog.Warningf("No VFS name stored for volume %s, skipping cache cleanup", mm.volumeID)
		return
	}

	// VFS cache is stored at ~/.cache/rclone/vfs/<vfsName>/
	// Since we mount the encrypted LUKS volume at /root/.cache/rclone, the path is:
	cacheDir := fmt.Sprintf("%s/vfs/%s", VFSCacheBasePath, mm.vfsName)

	klog.Infof("Cleaning up VFS cache directory for volume %s: %s", mm.volumeID, cacheDir)

	if err := os.RemoveAll(cacheDir); err != nil {
		klog.Warningf("Failed to remove VFS cache directory %s: %v", cacheDir, err)
	} else {
		klog.Infof("Successfully removed VFS cache directory for volume %s", mm.volumeID)
	}
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

