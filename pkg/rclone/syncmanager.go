package rclone

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/klog"
)

// VFSCacheConfig holds VFS cache and directory-metadata configuration options
type VFSCacheConfig struct {
	CacheMode         string // off, minimal, writes, full
	CacheMaxAge       string // e.g., "1h", "24h"
	CacheMaxSize      string // e.g., "10G", "100M"
	CachePollInterval string // e.g., "1m", "5m" - how often to poll for stale cache entries
	WriteBack         string // e.g., "5s", "0" for immediate
	DirCacheTime      string // cache directory listings, e.g. "5m", "1h"
	AttrTimeout       string // cache file attributes (stat), e.g. "5m", "1h"
}

// DefaultVFSCacheConfig returns sensible defaults for VFS caching
func DefaultVFSCacheConfig() *VFSCacheConfig {
	return &VFSCacheConfig{
		CacheMode:         "full",
		CacheMaxAge:       "4h", // 4 hours to handle long-running operations
		CacheMaxSize:      "5G", // 5GB to handle large files
		CachePollInterval: "1m", // Poll every minute for stale cache entries
		WriteBack:         "3s", // Start uploads quickly to reduce cache pressure
		// 1h metadata caching avoids an S3 ListObjects+decrypt on every stat/
		// readdir; safe for these RWO/single-writer volumes. Override per SC.
		DirCacheTime: "1h",
		AttrTimeout:  "1h",
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
	vfsName         string        // Deterministic VFS name derived from volumeID
	s3ConfigName    string        // Named rclone config for S3 backend
	cryptConfigName string        // Named rclone config for crypt layer
	uid             *int64        // UID for FUSE mount (from fsGroup)
	gid             *int64        // GID for FUSE mount (from fsGroup)
	stopCacheMon    chan struct{} // Signals the background cache monitor to stop
	stopMonOnce     sync.Once     // Guards stopCacheMon close so it is safe to call from both Unmount and reconcile
	refreshMu       sync.Mutex    // Serializes refreshVFS calls to prevent concurrent forget/refresh races
}

// StopCacheMonitor stops the background cache monitor without unmounting.
// Idempotent. Call before dropping a stale manager so its monitor doesn't keep
// evicting a cache dir that a replacement mount now owns.
func (mm *MountManager) StopCacheMonitor() {
	if mm.stopCacheMon != nil {
		mm.stopMonOnce.Do(func() { close(mm.stopCacheMon) })
	}
}

// NewMountManager creates a new rclone mount manager
// s3PathPrefix is optional - if empty, defaults to "volumes/{volumeID}/files"
// If s3PathPrefix is provided, path becomes "{s3PathPrefix}/volumes/{volumeID}/files"
func NewMountManager(s3Config *S3Config, volumeID, mountPoint string, vfsConfig *VFSCacheConfig, s3PathPrefix string, luksPassphrase string, fsGroup *int64) (*MountManager, error) {
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
		uid:             fsGroup,
		gid:             fsGroup,
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

	// Fallback only if a StorageClass passes an invalid duration; matches the 1h
	// DefaultVFSCacheConfig.
	const defaultCacheNs = int64(3600000000000) // 1h
	mountOpt := map[string]interface{}{
		"AllowOther":    true,
		"AllowNonEmpty": true,
		"DirCacheTime":  durationOrDefault(mm.vfsConfig.DirCacheTime, defaultCacheNs),
		"AttrTimeout":   durationOrDefault(mm.vfsConfig.AttrTimeout, defaultCacheNs),
	}

	// Set UID/GID on the FUSE mount so files appear owned by the pod's fsGroup,
	// allowing non-root containers to read, write, and delete files.
	if mm.uid != nil {
		mountOpt["UID"] = uint32(*mm.uid)
		klog.Infof("Setting FUSE mount UID to %d for volume %s", *mm.uid, mm.volumeID)
	}
	if mm.gid != nil {
		mountOpt["GID"] = uint32(*mm.gid)
		klog.Infof("Setting FUSE mount GID to %d for volume %s", *mm.gid, mm.volumeID)
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

	// Verify the FUSE mount is actually responding before declaring success.
	// The mount RPC may return before the FUSE daemon is fully initialized,
	// causing pods to see "transport endpoint not connected" errors.
	if err := mm.waitForMountReady(); err != nil {
		klog.Warningf("Mount readiness check failed for volume %s: %v (mount may still work)", mm.volumeID, err)
	}

	mm.stopCacheMon = make(chan struct{})
	go mm.cacheMonitor()

	// Stale cache from unclean shutdown: async refresh reconciles rclone's
	// in-memory state with S3 while dirty files remain available for re-upload.
	if mm.hasStaleVFSCache() {
		klog.Infof("Volume %s: stale VFS cache detected, triggering async VFS refresh", mm.volumeID)
		go mm.refreshVFS()
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

	mm.StopCacheMonitor()

	// Drain before unmounting: mount/unmount cancels rclone's VFS context, which
	// aborts all in-flight uploads mid-stream and leaves partial objects in S3.
	// false → drain unconfirmed; keep VFS cache so rclone can retry on next mount.
	drained := mm.waitForPendingUploads()

	params := map[string]interface{}{"mountPoint": mm.mountPoint}
	_, err := RPC("mount/unmount", params)
	if err != nil {
		if strings.Contains(err.Error(), "mount not found") {
			// rclone self-unmounted (e.g. VFS error); already gone, not a failure.
			klog.Infof("Volume %s: mount already gone when calling mount/unmount — rclone self-unmounted", mm.volumeID)
		} else {
			klog.Warningf("mount/unmount RPC failed: %v", err)
			_, err = RPC("mount/unmountall", map[string]interface{}{})
			if err != nil {
				klog.Warningf("Failed to unmount rclone: %v", err)
			}
		}
	}

	DeleteNamedConfigs(mm.cryptConfigName, mm.s3ConfigName)

	if drained {
		mm.cleanupVFSCacheDir()
	} else {
		klog.Infof("Volume %s: preserving VFS cache for retry on next mount", mm.volumeID)
	}

	mm.mounted = false

	klog.Infof("Successfully unmounted encrypted S3 volume %s", mm.volumeID)
	return nil
}

// waitForPendingUploads polls vfs/stats until the write-back queue is empty.
// Returns true only when the queue is confirmed empty; false otherwise (the
// caller must preserve the local VFS cache for retry on next mount).
// RPC failures are retried indefinitely while the FUSE mount is alive — a
// lost RC connection is not evidence the queue is empty.
func (mm *MountManager) waitForPendingUploads() bool {
	klog.Infof("Waiting for pending uploads to complete for volume %s", mm.volumeID)

	writeBackWait := 5
	if mm.vfsConfig.WriteBack != "" {
		if ns, err := parseDurationToNs(mm.vfsConfig.WriteBack); err == nil {
			writeBackWait = int(ns / 1e9)
		}
	}
	writeBackWait += 2
	klog.Infof("Waiting %d seconds for write-back to flush for volume %s", writeBackWait, mm.volumeID)
	time.Sleep(time.Duration(writeBackWait) * time.Second)

	maxWait := 6 * time.Hour
	pollInterval := 2 * time.Second
	logInterval := 30 * time.Second
	deadline := time.Now().Add(maxWait)
	lastLog := time.Now()
	consecutiveRPCFailures := 0

	fsName := mm.cryptConfigName + ":"
	for time.Now().Before(deadline) {
		// Self-unmount: rclone tore down the FUSE and cancelled all in-flight uploads.
		if !mm.isMountPoint() {
			klog.Errorf("Volume %s: rclone FUSE mount disappeared while waiting for uploads to drain — "+
				"in-flight uploads were cancelled; local VFS cache will be preserved for retry on next mount", mm.volumeID)
			return false
		}

		result, err := RPC("vfs/stats", map[string]interface{}{"fs": fsName})
		if err != nil {
			// "no VFS found" is terminal, not transient: the kernel mount exists
			// but this librclone instance never created its VFS (orphaned after a
			// driver restart). Retrying can never succeed, so stop and preserve
			// the on-disk cache for the next mount to resume.
			if isNoVFSError(err) {
				klog.Warningf("Volume %s: mount has no VFS in this driver instance (orphaned after a restart); "+
					"stopping drain, preserving cache for re-mount", mm.volumeID)
				return false
			}
			consecutiveRPCFailures++
			if consecutiveRPCFailures == 1 || consecutiveRPCFailures%5 == 0 {
				klog.Warningf("Volume %s: vfs/stats RPC failing (consecutive failures: %d): %v — "+
					"retrying to protect locally cached data", mm.volumeID, consecutiveRPCFailures, err)
			}
			time.Sleep(pollInterval)
			continue
		}
		consecutiveRPCFailures = 0

		inProgress, queued := int64(0), int64(0)
		if result != nil && result.Output != nil {
			if dc, ok := result.Output["diskCache"].(map[string]interface{}); ok {
				if v, ok := dc["uploadsInProgress"].(float64); ok {
					inProgress = int64(v)
				}
				if v, ok := dc["uploadsQueued"].(float64); ok {
					queued = int64(v)
				}
			}
		}

		if inProgress == 0 && queued == 0 {
			klog.Infof("Volume %s: upload queue empty, proceeding with unmount", mm.volumeID)
			return true
		}
		if time.Since(lastLog) >= logInterval {
			klog.Infof("Volume %s: waiting for %d uploads in progress, %d queued", mm.volumeID, inProgress, queued)
			lastLog = time.Now()
		}
		time.Sleep(pollInterval)
	}

	klog.Warningf("Volume %s: upload drain timed out after %s — local VFS cache will be preserved for retry on next mount", mm.volumeID, maxWait)
	return false
}

// IsUploadQueueEmpty does a single non-blocking poll of vfs/stats.
// Returns true if there are no uploads in progress or queued.
// Use waitForPendingUploads to block until the queue drains.
func (mm *MountManager) IsUploadQueueEmpty() bool {
	if !mm.isMountPoint() {
		return true
	}
	result, err := RPC("vfs/stats", map[string]interface{}{"fs": mm.cryptConfigName + ":"})
	if err != nil {
		// Orphaned mount (no VFS in this instance): nothing to drain here, take
		// the fast unmount path rather than starting an endless background drain.
		if isNoVFSError(err) {
			return true
		}
		return false // assume work pending on other RPC errors
	}
	if result != nil && result.Output != nil {
		if dc, ok := result.Output["diskCache"].(map[string]interface{}); ok {
			inProgress, queued := int64(0), int64(0)
			if v, ok := dc["uploadsInProgress"].(float64); ok {
				inProgress = int64(v)
			}
			if v, ok := dc["uploadsQueued"].(float64); ok {
				queued = int64(v)
			}
			return inProgress == 0 && queued == 0
		}
	}
	return false
}

func (mm *MountManager) isMountPoint() bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		klog.Infof("Failed to read /proc/mounts: %v", err)
		return false
	}

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == mm.mountPoint {
			klog.Infof("Found mount at %s: %s", mm.mountPoint, line)
			return true
		}
	}

	return false
}

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
// Serialized with a mutex to prevent concurrent forget/refresh races from
// the cache monitor and mount paths.
func (mm *MountManager) refreshVFS() {
	mm.refreshMu.Lock()
	defer mm.refreshMu.Unlock()

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

// waitForMountReady verifies the FUSE mount is responding by stat-ing the
// mount point. Retries for up to 5 seconds to allow the FUSE daemon to
// fully initialize after the mount RPC returns.
func (mm *MountManager) waitForMountReady() error {
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error

	for time.Now().Before(deadline) {
		_, err := os.ReadDir(mm.mountPoint)
		if err == nil {
			klog.Infof("FUSE mount verified ready for volume %s at %s", mm.volumeID, mm.mountPoint)
			return nil
		}
		lastErr = err
		klog.Infof("FUSE mount not ready yet for volume %s: %v, retrying...", mm.volumeID, err)
		time.Sleep(250 * time.Millisecond)
	}

	return fmt.Errorf("FUSE mount not ready after 5s for volume %s: %v", mm.volumeID, lastErr)
}

// cleanupVFSCacheDir removes the on-disk VFS cache directories after unmount.
// This is safe because it is only called from Unmount() after rclone's own graceful
// unmount has flushed the write-back queue to S3. On a hard restart/crash,
// Unmount() never runs so the cache survives for rclone to resume on remount.
// Without this cleanup, cache data accumulates indefinitely because rclone's
// CacheMaxSize eviction only runs while the VFS is active.
func (mm *MountManager) cleanupVFSCacheDir() {
	if mm.vfsName == "" {
		return
	}

	cacheDir := fmt.Sprintf("%s/vfs/%s", VFSCacheBasePath, mm.vfsName)
	metaDir := fmt.Sprintf("%s/vfsMeta/%s", VFSCacheBasePath, mm.vfsName)

	if err := os.RemoveAll(cacheDir); err != nil {
		klog.Warningf("Failed to remove VFS cache dir %s: %v", cacheDir, err)
	} else {
		klog.Infof("Removed VFS cache dir %s for volume %s", cacheDir, mm.volumeID)
	}
	if err := os.RemoveAll(metaDir); err != nil {
		klog.Warningf("Failed to remove VFS meta dir %s: %v", metaDir, err)
	}

	RemoveVFSName(mm.volumeID)
}

// cacheMonitor runs in the background while the volume is mounted.
// It periodically checks the on-disk VFS cache size and, when it exceeds
// CacheMaxSize, removes the oldest already-uploaded files. This handles
// the case where rclone's built-in eviction is insufficient (e.g. long-lived
// mounts where CacheMaxAge resets on every file access).
func (mm *MountManager) cacheMonitor() {
	if mm.vfsName == "" {
		return
	}

	// Parse the configured max size; if not set, nothing to enforce
	var maxBytes int64
	if mm.vfsConfig.CacheMaxSize != "" {
		parsed, err := parseSizeToBytes(mm.vfsConfig.CacheMaxSize)
		if err != nil || parsed <= 0 {
			return
		}
		maxBytes = parsed
	} else {
		return
	}

	// Poll every CachePollInterval (default 1m), or fall back to 1m
	pollInterval := time.Minute
	if mm.vfsConfig.CachePollInterval != "" {
		if ns, err := parseDurationToNs(mm.vfsConfig.CachePollInterval); err == nil && ns > 0 {
			pollInterval = time.Duration(ns)
		}
	}

	cacheDir := fmt.Sprintf("%s/vfs/%s", VFSCacheBasePath, mm.vfsName)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-mm.stopCacheMon:
			return
		case <-ticker.C:
			mm.evictCacheIfNeeded(cacheDir, maxBytes)
		}
	}
}

// evictCacheIfNeeded checks the cache directory size and removes the oldest
// files until the total is back under maxBytes. It only removes files that
// have no open file descriptors and are not actively being transferred.
func (mm *MountManager) evictCacheIfNeeded(cacheDir string, maxBytes int64) {
	// Never evict while anything is dirty: write-back-queued files aren't
	// "transferring" yet but aren't on S3 either.
	if mm.hasActiveTransfers() || !mm.IsUploadQueueEmpty() {
		return
	}

	// Collect all files with their sizes and modification times
	type cachedFile struct {
		path    string
		size    int64
		modTime time.Time
	}
	var files []cachedFile
	var totalSize int64

	_ = filepath.WalkDir(cacheDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		totalSize += info.Size()
		files = append(files, cachedFile{path: path, size: info.Size(), modTime: info.ModTime()})
		return nil
	})

	if totalSize <= maxBytes {
		return
	}

	klog.Infof("VFS cache for volume %s is %d bytes (limit %d), evicting oldest files",
		mm.volumeID, totalSize, maxBytes)

	// Sort oldest first
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})

	for _, f := range files {
		if totalSize <= maxBytes {
			break
		}
		// Skip files that have open file descriptors — removing them while
		// rclone holds a handle leaves the VFS item with didClose=false,
		// causing "internal error: didn't Close file" on the next access.
		if isCacheFileOpen(f.path) {
			klog.V(4).Infof("Skipping eviction of open cache file %s", f.path)
			continue
		}
		if err := os.Remove(f.path); err != nil {
			klog.V(4).Infof("Could not remove cache file %s: %v", f.path, err)
			continue
		}
		totalSize -= f.size
	}

	klog.Infof("VFS cache for volume %s reduced to %d bytes", mm.volumeID, totalSize)

	// After removing cache data files, clear rclone's in-memory directory
	// cache so it doesn't serve stale entries pointing to removed files.
	// On the next read rclone will re-download from S3 as needed.
	mm.refreshVFS()
}

// isCacheFileOpen reports whether any process has an open file descriptor
// pointing at path. It scans /proc/*/fd symlinks, the same technique used by
// lsof. Returns true on any error so callers err on the side of safety.
func isCacheFileOpen(path string) bool {
	// Resolve to canonical path so symlinks don't fool the comparison.
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		real = path
	}

	fdDirs, err := filepath.Glob("/proc/*/fd")
	if err != nil {
		return true
	}
	for _, fdDir := range fdDirs {
		entries, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			target, err := os.Readlink(filepath.Join(fdDir, e.Name()))
			if err != nil {
				continue
			}
			if target == real || target == path {
				return true
			}
		}
	}
	return false
}

// hasActiveTransfers returns true if rclone is currently uploading files
func (mm *MountManager) hasActiveTransfers() bool {
	result, err := RPC("core/stats", map[string]interface{}{})
	if err != nil {
		return true // Assume busy on error
	}
	if result == nil || result.Output == nil {
		return false
	}
	if t, ok := result.Output["transferring"]; ok {
		if tList, ok := t.([]interface{}); ok && len(tList) > 0 {
			return true
		}
	}
	if c, ok := result.Output["checking"]; ok {
		if cList, ok := c.([]interface{}); ok && len(cList) > 0 {
			return true
		}
	}
	return false
}

// IsMounted returns whether the volume is currently mounted
func (mm *MountManager) IsMounted() bool {
	return mm.mounted && mm.isMountPoint()
}

// isNoVFSError reports whether an rclone RPC error means this librclone instance
// has no VFS for the mount — a terminal state for an orphaned kernel mount left
// by a previous driver instance, not a transient RPC failure.
func isNoVFSError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no VFS found")
}

// durationOrDefault parses a duration string to nanoseconds, falling back to
// defaultNs when empty or invalid.
func durationOrDefault(s string, defaultNs int64) int64 {
	if s == "" {
		return defaultNs
	}
	if ns, err := parseDurationToNs(s); err == nil && ns > 0 {
		return ns
	}
	klog.Warningf("Invalid duration %q, using default", s)
	return defaultNs
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
