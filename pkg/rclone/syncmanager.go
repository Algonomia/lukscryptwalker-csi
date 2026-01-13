package rclone

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/lukscryptwalker-csi/pkg/luks"
	"k8s.io/klog"
)

// Global mount mutex to ensure mount operations are sequential
// This prevents race conditions when multiple volumes are mounted simultaneously
var globalMountMu sync.Mutex

// VFSCacheConfig holds VFS cache configuration options
type VFSCacheConfig struct {
	CacheMode    string // off, minimal, writes, full
	CacheMaxAge  string // e.g., "1h", "24h"
	CacheMaxSize string // e.g., "10G", "100M"
	WriteBack    string // e.g., "5s", "0" for immediate
}

// DefaultVFSCacheConfig returns sensible defaults for VFS caching
func DefaultVFSCacheConfig() *VFSCacheConfig {
	return &VFSCacheConfig{
		CacheMode:    "full",
		CacheMaxAge:  "1h",
		CacheMaxSize: "10G",
		WriteBack:    "5s",
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
	cryptRemote    string // The crypt remote string (e.g., :crypt{...}:) used for VFS operations
	mounted        bool
	mutex          sync.RWMutex
	luksPassphrase string
	luksManager    *luks.LUKSManager
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
	}

	klog.Infof("Created rclone mount manager for volume %s at %s (s3Path: %s)", volumeID, mountPoint, s3BasePath)
	return manager, nil
}

// Mount mounts the encrypted S3 remote at the mount point using librclone
func (mm *MountManager) Mount() error {
	// First acquire the global mount mutex to serialize mount operations
	globalMountMu.Lock()
	defer globalMountMu.Unlock()

	// Then acquire the instance mutex
	mm.mutex.Lock()
	defer mm.mutex.Unlock()

	if mm.mounted && mm.isMountPoint() {
		klog.Infof("Volume %s already mounted at %s", mm.volumeID, mm.mountPoint)
		return nil
	}

	klog.Infof("Mounting encrypted S3 volume %s at %s", mm.volumeID, mm.mountPoint)

	// Check for stale mount and try to clean it up
	if mm.isMountPoint() {
		klog.Warningf("Found stale mount at %s, attempting to unmount first", mm.mountPoint)
		// Try to unmount via librclone
		_, _ = RPC("mount/unmount", map[string]interface{}{"mountPoint": mm.mountPoint})
		// Use lazy unmount as fallback
		if mm.isMountPoint() {
			klog.Warningf("librclone unmount didn't work, trying umount -l %s", mm.mountPoint)
			cmd := exec.Command("umount", "-l", mm.mountPoint)
			if err := cmd.Run(); err != nil {
				klog.Warningf("umount -l failed: %v, will try mounting anyway with AllowNonEmpty", err)
			}
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

	klog.V(4).Infof("Calling mount/mount RPC for volume %s", mm.volumeID)

	_, err = RPC("mount/mount", params)
	if err != nil {
		return fmt.Errorf("failed to mount: %w", err)
	}

	// Resolve the actual VFS name that rclone assigned (may have suffix like [0], [1] for multiple mounts)
	vfsName := mm.resolveVFSName(cryptRemote)
	mm.cryptRemote = vfsName
	mm.mounted = true
	klog.Infof("Successfully mounted encrypted S3 volume %s at %s", mm.volumeID, mm.mountPoint)
	return nil
}

// resolveVFSName finds the actual VFS name assigned by rclone for our mount.
// When multiple VFS instances exist for similar remotes, rclone assigns suffixes like [0], [1].
// This function queries vfs/list to find the correct VFS name for our mount.
func (mm *MountManager) resolveVFSName(cryptRemote string) string {
	result, err := RPC("vfs/list", map[string]interface{}{})
	if err != nil {
		klog.V(4).Infof("vfs/list failed, using original remote: %v", err)
		return cryptRemote
	}

	if result == nil || result.Output == nil {
		return cryptRemote
	}

	// vfs/list returns {"vfses": ["remote1:", "remote2:[0]", "remote2:[1]", ...]}
	vfses, ok := result.Output["vfses"]
	if !ok {
		return cryptRemote
	}

	vfsList, ok := vfses.([]interface{})
	if !ok {
		return cryptRemote
	}

	// Look for exact match first
	for _, v := range vfsList {
		vfsName, ok := v.(string)
		if !ok {
			continue
		}
		if vfsName == cryptRemote {
			klog.V(4).Infof("Found exact VFS match for volume %s", mm.volumeID)
			return vfsName
		}
	}

	// Look for suffixed version (e.g., cryptRemote + "[0]", "[1]", etc.)
	// Find the highest suffix to get the most recently created one
	var bestMatch string
	highestSuffix := -1
	for _, v := range vfsList {
		vfsName, ok := v.(string)
		if !ok {
			continue
		}
		// Check if this VFS name starts with our remote (accounting for potential suffix)
		if len(vfsName) >= len(cryptRemote) && vfsName[:len(cryptRemote)] == cryptRemote {
			// Extract suffix number if present
			suffix := vfsName[len(cryptRemote):]
			if suffix == "" {
				if highestSuffix < 0 {
					bestMatch = vfsName
					highestSuffix = 0
				}
			} else if len(suffix) >= 3 && suffix[0] == '[' && suffix[len(suffix)-1] == ']' {
				// Parse [N] suffix
				var num int
				if _, err := fmt.Sscanf(suffix, "[%d]", &num); err == nil {
					if num > highestSuffix {
						bestMatch = vfsName
						highestSuffix = num
					}
				}
			}
		}
	}

	if bestMatch != "" {
		klog.V(4).Infof("Found VFS match with suffix: %s (for volume %s)", bestMatch, mm.volumeID)
		return bestMatch
	}

	klog.V(4).Infof("No VFS match found, using original remote for volume %s", mm.volumeID)
	return cryptRemote
}

// buildVFSOpt builds VFS options for the mount
func (mm *MountManager) buildVFSOpt() map[string]interface{} {
	vfsOpt := map[string]interface{}{
		"ReadChunkSize":      33554432, // 32M in bytes
		"ReadChunkSizeLimit": -1,       // off
		"BufferSize":         33554432, // 32M in bytes
		"Links":              true,     // Enable symlink support
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

	return vfsOpt
}

// Unmount unmounts the S3 volume using librclone
func (mm *MountManager) Unmount() error {
	mm.mutex.Lock()
	defer mm.mutex.Unlock()

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

	maxWaitTime := 300 // 5 minutes max wait
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

	// Also try to flush the VFS cache before unmounting
	if mm.cryptRemote != "" {
		_, err := RPC("vfs/refresh", map[string]interface{}{
			"fs":        mm.cryptRemote,
			"recursive": "true",
		})
		if err != nil {
			klog.V(4).Infof("vfs/refresh returned error (may be normal if already unmounted): %v", err)
		}
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

// IsMounted returns whether the volume is currently mounted
func (mm *MountManager) IsMounted() bool {
	mm.mutex.RLock()
	defer mm.mutex.RUnlock()
	return mm.mounted && mm.isMountPoint()
}

// ForceSync flushes any cached writes
func (mm *MountManager) ForceSync() error {
	// Skip if we don't have the cryptRemote stored (mount didn't complete)
	if mm.cryptRemote == "" {
		klog.V(4).Infof("No cryptRemote stored for volume %s, skipping vfs/refresh", mm.volumeID)
		return nil
	}

	// With rclone mount via librclone, we can use vfs/refresh to ensure cache is flushed
	// Note: VFS operations require the remote string (e.g., :crypt{...}:), not the mount path
	params := map[string]interface{}{
		"fs":        mm.cryptRemote,
		"recursive": "true",
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

