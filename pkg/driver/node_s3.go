package driver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/lukscryptwalker-csi/pkg/rclone"
	"github.com/lukscryptwalker-csi/pkg/secrets"
	"k8s.io/klog"
)

// S3 Storage Constants
const (
	StorageBackendParam   = "storage-backend"
	S3BucketParam         = "s3-bucket"
	S3RegionParam         = "s3-region"
	S3EndpointParam       = "s3-endpoint"
	S3ForcePathStyleParam = "s3-force-path-style"
	S3PathPrefixParam     = "s3-path-prefix" // Custom path prefix in S3 (default: volumes/{volumeID}/files)
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
	pv, err := ns.getPVByVolumeID(ctx, volumeID)
	if err != nil {
		return fmt.Errorf("failed to get PV for volume %s: %v", volumeID, err)
	}

	scParams, err := ns.getStorageClassParameters(ctx, pv.Spec.StorageClassName)
	if err != nil {
		return fmt.Errorf("failed to get StorageClass parameters: %v", err)
	}

	// Extract secret parameters and fetch from K8s
	secretParams := secrets.ExtractSecretParams(scParams, volumeContext)
	volSecrets, err := ns.secretsManager.FetchVolumeSecrets(ctx, secretParams)
	if err != nil {
		return fmt.Errorf("failed to fetch secrets: %v", err)
	}

	// Build S3 config with credentials from secrets
	s3Config, err := ns.getS3ConfigFromContextWithSecrets(volumeContext, volSecrets)
	if err != nil {
		return fmt.Errorf("failed to get S3 configuration: %v", err)
	}

	// Use passphrase from fetched secrets
	passphrase := volSecrets.Passphrase
	if passphrase == "" {
		return fmt.Errorf("LUKS passphrase not found in secrets")
	}

	// Extract VFS cache configuration from StorageClass/volume context
	vfsConfig := ns.getVFSCacheConfig(volumeContext)

	// Extract custom S3 path prefix if provided
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

// getS3ConfigFromContextWithSecrets extracts S3 configuration using VolumeSecrets
func (ns *NodeServer) getS3ConfigFromContextWithSecrets(volumeContext map[string]string, volSecrets *secrets.VolumeSecrets) (*rclone.S3Config, error) {
	config := &rclone.S3Config{}

	// Required parameters
	bucket, exists := volumeContext[S3BucketParam]
	if !exists {
		return nil, fmt.Errorf("s3-bucket parameter is required")
	}
	config.Bucket = bucket

	region, exists := volumeContext[S3RegionParam]
	if !exists {
		return nil, fmt.Errorf("s3-region parameter is required")
	}
	config.Region = region

	// Optional parameters
	if endpoint, exists := volumeContext[S3EndpointParam]; exists {
		config.Endpoint = endpoint
	}

	if forcePathStyle, exists := volumeContext[S3ForcePathStyleParam]; exists {
		if parsed, err := strconv.ParseBool(forcePathStyle); err == nil {
			config.ForcePathStyle = parsed
		}
	}

	// Get credentials from VolumeSecrets
	config.AccessKeyID = volSecrets.S3AccessKeyID
	config.SecretAccessKey = volSecrets.S3SecretAccessKey

	klog.V(4).Infof("S3 config built: bucket=%s, region=%s, endpoint=%s, hasCredentials=%v",
		config.Bucket, config.Region, config.Endpoint, config.AccessKeyID != "")

	return config, nil
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
