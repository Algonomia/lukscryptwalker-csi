package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lukscryptwalker-csi/pkg/rclone"
	"github.com/lukscryptwalker-csi/pkg/secrets"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"
)

// S3 Storage Constants
const (
	StorageBackendParam = "storage-backend"
	S3PathPrefixParam   = "s3-path-prefix" // Custom path prefix in S3 bucket
	// VFS Cache Parameters (for rclone mount mode)
	VFSCacheModeParam    = "rclone-vfs-cache-mode"     // off, minimal, writes, full
	VFSCacheMaxAgeParam  = "rclone-vfs-cache-max-age"  // e.g., "1h", "24h"
	VFSCacheMaxSizeParam = "rclone-vfs-cache-max-size" // e.g., "10G", "100M"
	VFSWriteBackParam    = "rclone-vfs-write-back"     // e.g., "5s", "0"
	VFSCacheDirParam     = "rclone-cache-dir"          // directory for cache files
)

// S3SyncManager holds mount managers for active S3 volumes
type S3SyncManager struct {
	mountManagers map[string]*rclone.MountManager // volumeID -> mount manager
	mutex         sync.RWMutex
}

// NewS3SyncManager creates a new S3 sync manager
func NewS3SyncManager() *S3SyncManager {
	return &S3SyncManager{
		mountManagers: make(map[string]*rclone.MountManager),
	}
}

// isS3Backend checks if the volume is configured for S3 backend
func (ns *NodeServer) isS3Backend(volumeContext map[string]string) bool {
	storageBackend, exists := volumeContext[StorageBackendParam]
	return exists && storageBackend == "s3"
}

// setupS3Volume sets up an S3-only volume (no LUKS layer)
func (ns *NodeServer) setupS3Volume(params *StagingParameters, volumeContext, secrets map[string]string) error {
	klog.Infof("Setting up S3-only volume %s", params.volumeID)

	// Create staging directory (no filesystem, just a mount point)
	if err := os.MkdirAll(params.stagingTargetPath, 0777); err != nil {
		return fmt.Errorf("failed to create staging directory: %v", err)
	}

	// Apply fsGroup permissions to the staging directory
	if params.fsGroup != nil {
		if err := ns.applyFsGroupPermissions(params.stagingTargetPath, *params.fsGroup); err != nil {
			return fmt.Errorf("failed to apply fsGroup permissions to S3 staging path: %v", err)
		}
	}

	// Setup S3 sync with file encryption
	if err := ns.setupS3Sync(params.volumeID, params.stagingTargetPath, volumeContext, secrets); err != nil {
		return fmt.Errorf("failed to setup S3 sync: %v", err)
	}

	klog.Infof("Successfully set up S3-only volume %s", params.volumeID)
	return nil
}

// setupS3Sync initializes S3 mount for a volume using rclone mount mode
func (ns *NodeServer) setupS3Sync(volumeID, stagingPath string, volumeContext map[string]string, _ map[string]string) error {
	klog.Infof("Setting up S3 mount for volume %s", volumeID)
	ctx := context.Background()

	// Get StorageClass parameters for S3 credentials secret reference
	pv, err := getPVByVolumeID(ctx, ns.clientset, volumeID)
	if err != nil {
		return fmt.Errorf("failed to get PV for volume %s: %v", volumeID, err)
	}

	scParams, err := getStorageClassParameters(ctx, ns.clientset, pv.Spec.StorageClassName)
	if err != nil {
		return fmt.Errorf("failed to get StorageClass parameters: %v", err)
	}

	// Extract secret parameters and fetch from K8s
	secretParams := secrets.ExtractSecretParams(scParams, volumeContext)
	volSecrets, err := ns.secretsManager.FetchVolumeSecrets(ctx, secretParams)
	if err != nil {
		return fmt.Errorf("failed to fetch secrets: %v", err)
	}

	// Build S3 config from secrets (all S3 config is in the secret)
	s3Config := ns.getS3ConfigFromSecrets(volSecrets)
	if s3Config.Bucket == "" {
		return fmt.Errorf("S3 bucket not found in secrets")
	}

	// Use passphrase from fetched secrets
	passphrase := volSecrets.Passphrase
	if passphrase == "" {
		return fmt.Errorf("LUKS passphrase not found in secrets")
	}

	// Extract VFS cache configuration from StorageClass/volume context
	vfsConfig := ns.getVFSCacheConfig(volumeContext)

	// S3 path prefix is now a StorageClass parameter
	s3PathPrefix := volumeContext[S3PathPrefixParam]

	// Create rclone mount manager
	mountMgr, err := rclone.NewMountManager(s3Config, volumeID, stagingPath, passphrase, vfsConfig, s3PathPrefix)
	if err != nil {
		return fmt.Errorf("failed to create rclone mount manager: %v", err)
	}

	// Mount the encrypted S3 remote
	if err := mountMgr.Mount(); err != nil {
		return fmt.Errorf("failed to mount S3 volume: %v", err)
	}

	// Store mount manager
	ns.s3SyncMgr.mutex.Lock()
	ns.s3SyncMgr.mountManagers[volumeID] = mountMgr
	ns.s3SyncMgr.mutex.Unlock()

	klog.Infof("Successfully mounted S3 volume %s at %s", volumeID, stagingPath)
	return nil
}

// getVFSCacheConfig extracts VFS cache configuration from volume context
func (ns *NodeServer) getVFSCacheConfig(volumeContext map[string]string) *rclone.VFSCacheConfig {
	config := rclone.DefaultVFSCacheConfig()

	if cacheMode, exists := volumeContext[VFSCacheModeParam]; exists && cacheMode != "" {
		config.CacheMode = cacheMode
	}

	if cacheMaxAge, exists := volumeContext[VFSCacheMaxAgeParam]; exists && cacheMaxAge != "" {
		config.CacheMaxAge = cacheMaxAge
	}

	if cacheMaxSize, exists := volumeContext[VFSCacheMaxSizeParam]; exists && cacheMaxSize != "" {
		config.CacheMaxSize = cacheMaxSize
	}

	if writeBack, exists := volumeContext[VFSWriteBackParam]; exists && writeBack != "" {
		config.WriteBack = writeBack
	}

	if cacheDir, exists := volumeContext[VFSCacheDirParam]; exists && cacheDir != "" {
		config.CacheDir = cacheDir
	}

	klog.V(4).Infof("VFS cache config: mode=%s, maxAge=%s, maxSize=%s, writeBack=%s",
		config.CacheMode, config.CacheMaxAge, config.CacheMaxSize, config.WriteBack)

	return config
}

// getS3ConfigFromSecrets extracts S3 configuration from VolumeSecrets
// All S3 config (bucket, region, endpoint, credentials, etc.) is stored in a single secret
func (ns *NodeServer) getS3ConfigFromSecrets(volSecrets *secrets.VolumeSecrets) *rclone.S3Config {
	config := &rclone.S3Config{
		Bucket:          volSecrets.S3Bucket,
		Region:          volSecrets.S3Region,
		Endpoint:        volSecrets.S3Endpoint,
		ForcePathStyle:  volSecrets.S3ForcePathStyle,
		AccessKeyID:     volSecrets.S3AccessKeyID,
		SecretAccessKey: volSecrets.S3SecretAccessKey,
	}

	klog.V(4).Infof("S3 config from secrets: bucket=%s, region=%s, endpoint=%s, forcePathStyle=%v, hasCredentials=%v",
		config.Bucket, config.Region, config.Endpoint, config.ForcePathStyle, config.AccessKeyID != "")

	return config
}

// cleanupS3Sync unmounts S3 volume for cleanup
func (ns *NodeServer) cleanupS3Sync(volumeID string) error {
	klog.Infof("Cleaning up S3 mount for volume %s", volumeID)

	ns.s3SyncMgr.mutex.Lock()
	defer ns.s3SyncMgr.mutex.Unlock()

	// Unmount the S3 volume
	if mountMgr, exists := ns.s3SyncMgr.mountManagers[volumeID]; exists {
		// Force sync any cached writes before unmount
		if err := mountMgr.ForceSync(); err != nil {
			klog.Warningf("Failed to force sync before unmount for volume %s: %v", volumeID, err)
		}

		// Unmount the volume (this also tears down the encrypted cache mount)
		if err := mountMgr.Unmount(); err != nil {
			klog.Errorf("Failed to unmount S3 volume %s: %v", volumeID, err)
			return err
		}
		delete(ns.s3SyncMgr.mountManagers, volumeID)
	}

	// Delete cache backing file to free up disk space
	ns.cleanupCacheBackingFile(volumeID)

	klog.Infof("S3 mount cleanup completed for volume %s", volumeID)
	return nil
}

// restoreS3VolumeStaging restores an S3 volume's staging mount after node reboot
func (ns *NodeServer) restoreS3VolumeStaging(volumeID, stagingTargetPath string, volumeContext, secrets map[string]string) error {
	klog.Infof("Restoring S3 volume %s at %s", volumeID, stagingTargetPath)

	// Create staging directory if it doesn't exist
	if err := os.MkdirAll(stagingTargetPath, 0777); err != nil {
		return fmt.Errorf("failed to create staging directory: %v", err)
	}

	// Setup S3 sync
	if err := ns.setupS3Sync(volumeID, stagingTargetPath, volumeContext, secrets); err != nil {
		return fmt.Errorf("failed to setup S3 sync during restore: %v", err)
	}

	klog.Infof("Successfully restored S3 volume %s", volumeID)
	return nil
}

// cleanupStaleS3Mounts cleans up stale S3/FUSE mounts that may remain after
// an unclean shutdown (e.g., OOM kill). For S3 volumes, it attempts to restore
// the mount instead of just unmounting to keep existing pods working.
func (ns *NodeServer) cleanupStaleS3Mounts() {
	csiPluginPath := "/var/lib/kubelet/plugins/kubernetes.io/csi/" + DriverName
	klog.Infof("Checking for stale S3 mounts in %s", csiPluginPath)

	// Check if the CSI plugin directory exists
	if _, err := os.Stat(csiPluginPath); os.IsNotExist(err) {
		klog.V(4).Infof("CSI plugin path %s does not exist, no cleanup needed", csiPluginPath)
		return
	}

	// List all volume hash directories
	entries, err := os.ReadDir(csiPluginPath)
	if err != nil {
		klog.Warningf("Failed to read CSI plugin directory %s: %v", csiPluginPath, err)
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		volumeDir := filepath.Join(csiPluginPath, entry.Name())
		globalmountPath := filepath.Join(volumeDir, "globalmount")

		// Check if globalmount exists - use Lstat to not follow symlinks
		_, statErr := os.Lstat(globalmountPath)
		if os.IsNotExist(statErr) {
			continue
		}

		// Check if it's a stale FUSE mount (transport endpoint not connected)
		_, err := os.Stat(globalmountPath)
		isStaleFUSE := err != nil && (strings.Contains(err.Error(), "transport endpoint is not connected") ||
			strings.Contains(err.Error(), "stale file handle"))

		if !isStaleFUSE {
			// Not a stale mount, skip
			continue
		}

		klog.Infof("Detected stale FUSE mount at %s", globalmountPath)

		// Try to get volume ID from vol_data.json (written by kubelet)
		volumeID := ns.getVolumeIDFromVolData(volumeDir)
		if volumeID == "" {
			klog.Warningf("Could not determine volume ID for %s, will unmount only", volumeDir)
			ns.unmountStaleS3Mount(globalmountPath)
			continue
		}

		klog.Infof("Found stale S3 mount for volume %s at %s", volumeID, globalmountPath)

		// Get volume context from the PV
		ctx := context.Background()
		volumeContext := ns.getS3VolumeContext(ctx, volumeID)
		if volumeContext == nil {
			klog.Warningf("Could not get volume context for %s, will unmount only", volumeID)
			ns.unmountStaleS3Mount(globalmountPath)
			continue
		}

		// Try to restore the S3 mount
		klog.Infof("Attempting to restore S3 volume %s at %s", volumeID, globalmountPath)

		// First unmount the stale FUSE mount
		umountCmd := exec.Command("umount", "-l", globalmountPath)
		if err := umountCmd.Run(); err != nil {
			klog.Warningf("umount -l failed for stale FUSE mount %s: %v", globalmountPath, err)
		}

		// Try to restore the S3 mount
		if err := ns.restoreS3VolumeStaging(volumeID, globalmountPath, volumeContext, nil); err != nil {
			klog.Errorf("Failed to restore S3 volume %s: %v - pods using this volume will need restart", volumeID, err)
			// Clean up the directory so kubelet can recreate it later
			os.RemoveAll(globalmountPath)
		} else {
			klog.Infof("Successfully restored S3 volume %s at %s", volumeID, globalmountPath)
		}
	}

	klog.Infof("Stale S3 mount cleanup completed")
}

// getVolumeIDFromVolData reads the volume ID from kubelet's vol_data.json file
func (ns *NodeServer) getVolumeIDFromVolData(volumeDir string) string {
	volDataPath := filepath.Join(volumeDir, "vol_data.json")
	data, err := os.ReadFile(volDataPath)
	if err != nil {
		klog.V(4).Infof("Could not read vol_data.json at %s: %v", volDataPath, err)
		return ""
	}

	// vol_data.json contains {"volumeHandle":"pvc-xxx", ...}
	var volData map[string]interface{}
	if err := json.Unmarshal(data, &volData); err != nil {
		klog.Warningf("Failed to parse vol_data.json at %s: %v", volDataPath, err)
		return ""
	}

	if volumeHandle, ok := volData["volumeHandle"].(string); ok {
		return volumeHandle
	}

	return ""
}

// getS3VolumeContext gets the volume context for an S3 volume from its PV
func (ns *NodeServer) getS3VolumeContext(ctx context.Context, volumeID string) map[string]string {
	if ns.clientset == nil {
		return nil
	}

	pv, err := ns.clientset.CoreV1().PersistentVolumes().Get(ctx, volumeID, metav1.GetOptions{})
	if err != nil {
		klog.Warningf("Failed to get PV %s: %v", volumeID, err)
		return nil
	}

	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != DriverName {
		return nil
	}

	volumeContext := pv.Spec.CSI.VolumeAttributes
	if volumeContext == nil {
		return nil
	}

	// Verify it's an S3 backend
	if !ns.isS3Backend(volumeContext) {
		return nil
	}

	return volumeContext
}

// unmountStaleS3Mount unmounts a stale FUSE mount and cleans up the directory
func (ns *NodeServer) unmountStaleS3Mount(mountPath string) {
	umountCmd := exec.Command("umount", "-l", mountPath)
	if err := umountCmd.Run(); err != nil {
		klog.Warningf("umount -l failed for %s: %v", mountPath, err)
	}

	if err := os.RemoveAll(mountPath); err != nil {
		klog.Warningf("Failed to remove directory %s: %v", mountPath, err)
	}
}

// cleanupCacheBackingFile removes the LUKS cache backing file for a volume
func (ns *NodeServer) cleanupCacheBackingFile(volumeID string) {
	cacheBasePath := rclone.DefaultCacheBasePath
	backingFile := filepath.Join(cacheBasePath, "backing", fmt.Sprintf("%s.luks", volumeID))
	mountPath := filepath.Join(cacheBasePath, "mounts", volumeID)

	// Remove backing file
	if err := os.Remove(backingFile); err != nil {
		if !os.IsNotExist(err) {
			klog.Warningf("Failed to remove cache backing file %s: %v", backingFile, err)
		}
	} else {
		klog.Infof("Removed cache backing file: %s", backingFile)
	}

	// Remove mount directory
	if err := os.RemoveAll(mountPath); err != nil {
		if !os.IsNotExist(err) {
			klog.Warningf("Failed to remove cache mount directory %s: %v", mountPath, err)
		}
	} else {
		klog.V(4).Infof("Removed cache mount directory: %s", mountPath)
	}
}
