package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/lukscryptwalker-csi/pkg/rclone"
	"github.com/lukscryptwalker-csi/pkg/secrets"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog"
)

// S3 Storage Constants
const (
	StorageBackendParam = "storage-backend"
	S3PathPrefixParam   = "s3-path-prefix" // Custom path prefix in S3 bucket
	// VFS Cache Parameters (for rclone mount mode)
	VFSCacheModeParam         = "rclone-vfs-cache-mode"          // off, minimal, writes, full
	VFSCacheMaxAgeParam       = "rclone-vfs-cache-max-age"       // e.g., "1h", "24h"
	VFSCacheMaxSizeParam      = "rclone-vfs-cache-max-size"      // e.g., "10G", "100M"
	VFSCachePollIntervalParam = "rclone-vfs-cache-poll-interval" // e.g., "1m", "5m"
	VFSWriteBackParam         = "rclone-vfs-write-back"          // e.g., "5s", "0"
	// Directory-metadata caching (mount options). Raise these for
	// metadata-heavy workloads on large directories (e.g. pgbackrest WAL).
	DirCacheTimeParam = "rclone-dir-cache-time" // e.g., "5m", "1h"
	AttrTimeoutParam  = "rclone-attr-timeout"   // e.g., "5m", "1h"
)

// S3SyncManager holds mount managers for active S3 volumes
type S3SyncManager struct {
	mountManagers    map[string]*rclone.MountManager
	volumesInSetup   map[string]bool
	mutex            sync.RWMutex
	setupMutex       sync.RWMutex
	backgroundDrains map[string]chan struct{} // volumeID → closed when drain completes
	pendingDrains    map[string]bool          // volumeIDs with drain pending from a previous driver instance
	drainMu          sync.Mutex
}

// NewS3SyncManager creates a new S3 sync manager
func NewS3SyncManager() *S3SyncManager {
	sm := &S3SyncManager{
		mountManagers:    make(map[string]*rclone.MountManager),
		volumesInSetup:   make(map[string]bool),
		backgroundDrains: make(map[string]chan struct{}),
		pendingDrains:    make(map[string]bool),
	}
	sm.loadPendingDrains()
	return sm
}

// loadPendingDrains reads drain-pending markers written by a previous driver
// instance and populates the in-memory pendingDrains map. Called once at startup.
func (sm *S3SyncManager) loadPendingDrains() {
	volumeIDs := rclone.ListDrainPending()
	if len(volumeIDs) == 0 {
		return
	}
	sm.drainMu.Lock()
	defer sm.drainMu.Unlock()
	for _, id := range volumeIDs {
		sm.pendingDrains[id] = true
		klog.Infof("Volume %s: found persistent drain-pending marker — VFS cache has unuploaded data from previous session", id)
	}
}

func (sm *S3SyncManager) startBackgroundDrain(volumeID string) {
	sm.drainMu.Lock()
	defer sm.drainMu.Unlock()
	sm.backgroundDrains[volumeID] = make(chan struct{})
	rclone.SaveDrainPending(volumeID)
}

func (sm *S3SyncManager) isBackgroundDraining(volumeID string) bool {
	sm.drainMu.Lock()
	defer sm.drainMu.Unlock()
	_, ok := sm.backgroundDrains[volumeID]
	return ok
}

func (sm *S3SyncManager) hasPendingDrain(volumeID string) bool {
	sm.drainMu.Lock()
	defer sm.drainMu.Unlock()
	return sm.pendingDrains[volumeID]
}

func (sm *S3SyncManager) finishBackgroundDrain(volumeID string) {
	sm.drainMu.Lock()
	defer sm.drainMu.Unlock()
	if done, ok := sm.backgroundDrains[volumeID]; ok {
		close(done)
		delete(sm.backgroundDrains, volumeID)
	}
	delete(sm.pendingDrains, volumeID)
	rclone.ClearDrainPending(volumeID)
}

func (sm *S3SyncManager) waitForBackgroundDrain(volumeID string, timeout time.Duration) {
	sm.drainMu.Lock()
	done, ok := sm.backgroundDrains[volumeID]
	sm.drainMu.Unlock()
	if !ok {
		return
	}
	select {
	case <-done:
	case <-time.After(timeout):
		klog.Warningf("Volume %s: timed out waiting for background drain", volumeID)
	}
}

// markVolumeSetupInProgress marks a volume as currently being set up
// This prevents the stale mount detector from interfering with setup
func (sm *S3SyncManager) markVolumeSetupInProgress(volumeID string) {
	sm.setupMutex.Lock()
	defer sm.setupMutex.Unlock()
	sm.volumesInSetup[volumeID] = true
	klog.V(4).Infof("Marked volume %s as setup in progress", volumeID)
}

// markVolumeSetupComplete removes the volume from the setup-in-progress set
func (sm *S3SyncManager) markVolumeSetupComplete(volumeID string) {
	sm.setupMutex.Lock()
	defer sm.setupMutex.Unlock()
	delete(sm.volumesInSetup, volumeID)
	klog.V(4).Infof("Marked volume %s setup as complete", volumeID)
}

// isVolumeSetupInProgress checks if a volume is currently being set up
func (sm *S3SyncManager) isVolumeSetupInProgress(volumeID string) bool {
	sm.setupMutex.RLock()
	defer sm.setupMutex.RUnlock()
	return sm.volumesInSetup[volumeID]
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

	// Setup S3 sync with file encryption (fsGroup is passed to rclone FUSE mount options)
	if err := ns.setupS3Sync(params.volumeID, params.stagingTargetPath, volumeContext, secrets, params.fsGroup); err != nil {
		return fmt.Errorf("failed to setup S3 sync: %v", err)
	}

	klog.Infof("Successfully set up S3-only volume %s", params.volumeID)
	return nil
}

// setupS3Sync initializes S3 mount for a volume using rclone mount mode.
// The caller is responsible for calling markVolumeSetupInProgress/markVolumeSetupComplete
// to guard the full publish flow (including bind mount) from stale mount detection.
func (ns *NodeServer) setupS3Sync(volumeID, stagingPath string, volumeContext map[string]string, _ map[string]string, fsGroup *int64) error {
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

	// If a background drain is still running (previous pod terminated with
	// uploads in progress), wait for it to finish before remounting.
	if ns.s3SyncMgr.isBackgroundDraining(volumeID) {
		klog.Infof("Volume %s: waiting for background drain before remounting", volumeID)
		ns.s3SyncMgr.waitForBackgroundDrain(volumeID, 30*time.Minute)
	}

	// Post-restart: a drain was in progress when the driver was killed. The
	// VFS cache has unuploaded data; Mount() will detect it via hasStaleVFSCache
	// and schedule a background refreshVFS. Clear the in-memory and disk markers
	// now since the stale-cache path takes ownership from here.
	if ns.s3SyncMgr.hasPendingDrain(volumeID) {
		klog.Infof("Volume %s: post-restart pending drain detected, Mount() will upload stale VFS cache data", volumeID)
		ns.s3SyncMgr.drainMu.Lock()
		delete(ns.s3SyncMgr.pendingDrains, volumeID)
		ns.s3SyncMgr.drainMu.Unlock()
		rclone.ClearDrainPending(volumeID)
	}

	// Create rclone mount manager
	mountMgr, err := rclone.NewMountManager(s3Config, volumeID, stagingPath, vfsConfig, s3PathPrefix, passphrase, fsGroup)
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

	if cachePollInterval, exists := volumeContext[VFSCachePollIntervalParam]; exists && cachePollInterval != "" {
		config.CachePollInterval = cachePollInterval
	}

	if writeBack, exists := volumeContext[VFSWriteBackParam]; exists && writeBack != "" {
		config.WriteBack = writeBack
	}

	if dirCacheTime, exists := volumeContext[DirCacheTimeParam]; exists && dirCacheTime != "" {
		config.DirCacheTime = dirCacheTime
	}

	if attrTimeout, exists := volumeContext[AttrTimeoutParam]; exists && attrTimeout != "" {
		config.AttrTimeout = attrTimeout
	}

	klog.V(4).Infof("VFS cache config: mode=%s, maxAge=%s, maxSize=%s, pollInterval=%s, writeBack=%s, dirCacheTime=%s, attrTimeout=%s",
		config.CacheMode, config.CacheMaxAge, config.CacheMaxSize, config.CachePollInterval, config.WriteBack,
		config.DirCacheTime, config.AttrTimeout)

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

// cleanupS3Sync unmounts an S3 volume. Returns (true, nil) when a background
// drain was started — the caller must skip staging cleanup so the FUSE mount
// stays live for the drain goroutine to finish uploading.
func (ns *NodeServer) cleanupS3Sync(volumeID string) (bool, error) {
	klog.Infof("Cleaning up S3 mount for volume %s", volumeID)

	// Kubelet retried NodeUnstageVolume while a previous drain is still running.
	if ns.s3SyncMgr.isBackgroundDraining(volumeID) {
		klog.Infof("Volume %s: background drain still in progress", volumeID)
		return true, nil
	}

	ns.s3SyncMgr.mutex.Lock()
	mountMgr, exists := ns.s3SyncMgr.mountManagers[volumeID]
	if !exists {
		ns.s3SyncMgr.mutex.Unlock()
		return false, nil
	}

	// Fast path: nothing pending, unmount synchronously.
	if mountMgr.IsUploadQueueEmpty() {
		defer ns.s3SyncMgr.mutex.Unlock()
		if err := mountMgr.Unmount(); err != nil {
			return false, err
		}
		delete(ns.s3SyncMgr.mountManagers, volumeID)
		return false, nil
	}
	ns.s3SyncMgr.mutex.Unlock()

	// Uploads in progress: drain in the background so NodeUnstageVolume returns
	// immediately, letting kubelet delete the pod and the StatefulSet schedule
	// a replacement without waiting for S3 transfers to complete.
	ns.s3SyncMgr.startBackgroundDrain(volumeID)
	go func() {
		defer ns.s3SyncMgr.finishBackgroundDrain(volumeID)
		klog.Infof("Volume %s: background drain started", volumeID)
		ns.s3SyncMgr.mutex.Lock()
		mm := ns.s3SyncMgr.mountManagers[volumeID]
		ns.s3SyncMgr.mutex.Unlock()
		if mm != nil {
			if err := mm.Unmount(); err != nil {
				klog.Errorf("Volume %s: background drain unmount failed: %v", volumeID, err)
			}
			ns.s3SyncMgr.mutex.Lock()
			delete(ns.s3SyncMgr.mountManagers, volumeID)
			ns.s3SyncMgr.mutex.Unlock()
		}
		klog.Infof("Volume %s: background drain complete", volumeID)
	}()

	return true, nil
}

// restoreS3VolumeStaging restores an S3 volume's staging mount after node reboot
func (ns *NodeServer) restoreS3VolumeStaging(volumeID, stagingTargetPath string, volumeContext, secrets map[string]string, volumeCapability *csi.VolumeCapability) error {
	klog.Infof("Restoring S3 volume %s at %s", volumeID, stagingTargetPath)

	// Create staging directory if it doesn't exist
	if err := os.MkdirAll(stagingTargetPath, 0777); err != nil {
		return fmt.Errorf("failed to create staging directory: %v", err)
	}

	// Extract fsGroup for the FUSE mount
	fsGroup := ns.extractFsGroup(volumeContext, volumeCapability)

	// Setup S3 sync
	if err := ns.setupS3Sync(volumeID, stagingTargetPath, volumeContext, secrets, fsGroup); err != nil {
		return fmt.Errorf("failed to setup S3 sync during restore: %v", err)
	}

	klog.Infof("Successfully restored S3 volume %s", volumeID)
	return nil
}

// resolveKubeletRoot resolves /var/lib/kubelet to its real path on the host.
// On microk8s, /var/lib/kubelet is a symlink to /var/snap/microk8s/common/var/lib/kubelet.
// We resolve in the host namespace using nsenter so the path matches what kubelet
// passes in CSI requests and what rclone uses for FUSE mounts.
func resolveKubeletRoot() string {
	cmd := exec.Command("nsenter", "-t", "1", "-m", "-u", "readlink", "-f", "/var/lib/kubelet")
	output, err := cmd.Output()
	if err != nil {
		return "/var/lib/kubelet"
	}
	resolved := strings.TrimSpace(string(output))
	if resolved == "" {
		return "/var/lib/kubelet"
	}
	return resolved
}

// cleanupStaleS3Mounts cleans up stale or missing S3/FUSE mounts that may remain after
// an unclean shutdown (e.g., OOM kill). For S3 volumes, it attempts to restore
// the mount instead of just unmounting to keep existing pods working.
func (ns *NodeServer) cleanupStaleS3Mounts() {
	kubeletRoot := resolveKubeletRoot()
	csiPluginPath := kubeletRoot + "/plugins/kubernetes.io/csi/" + DriverName
	klog.Infof("Checking for stale/missing S3 mounts in %s", csiPluginPath)

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

		// Probe with statfs, not stat: statfs is served locally by the FUSE layer
		// (no S3 ListObjects on a healthy mount), while a dead daemon still
		// surfaces as ENOTCONN/ESTALE.
		var st syscall.Statfs_t
		statErr := syscall.Statfs(globalmountPath, &st)
		if errors.Is(statErr, syscall.ENOENT) {
			// globalmount directory does not exist yet - nothing to clean up
			continue
		}

		// Check if it's a stale FUSE mount (transport endpoint not connected)
		isStaleFUSE := errors.Is(statErr, syscall.ENOTCONN) || errors.Is(statErr, syscall.ESTALE)

		// Check if mount is missing (directory exists but no FUSE mount)
		// This happens when the stale mount was already cleaned up but pods still need it
		isMissingMount := statErr == nil && !ns.isFUSEMountPoint(globalmountPath)

		if !isStaleFUSE && !isMissingMount {
			// Mount is healthy, skip
			continue
		}

		// Try to get volume ID from vol_data.json (written by kubelet)
		volumeID := ns.getVolumeIDFromVolData(volumeDir)
		if volumeID == "" {
			if isStaleFUSE {
				klog.Warningf("Could not determine volume ID for %s, will unmount only", volumeDir)
				ns.unmountStaleS3Mount(globalmountPath)
			}
			continue
		}

		if ns.s3SyncMgr.isVolumeSetupInProgress(volumeID) {
			klog.V(4).Infof("Volume %s is currently being set up, skipping stale detection", volumeID)
			continue
		}
		// Skip volumes whose drain goroutine is still live or whose drain persisted
		// across a driver restart — the VFS cache contains unuploaded data.
		if ns.s3SyncMgr.isBackgroundDraining(volumeID) || ns.s3SyncMgr.hasPendingDrain(volumeID) {
			klog.V(4).Infof("Volume %s has an active or post-restart pending drain, skipping stale detection", volumeID)
			continue
		}

		// Get volume context from the PV to check if it's an S3 volume
		ctx := context.Background()
		volumeContext := ns.getS3VolumeContext(ctx, volumeID)
		if volumeContext == nil {
			// Not an S3 volume or PV not found - skip for missing mounts, cleanup for stale
			if isStaleFUSE {
				klog.Warningf("Could not get volume context for %s, will unmount only", volumeID)
				ns.unmountStaleS3Mount(globalmountPath)
			}
			continue
		}

		if isStaleFUSE {
			klog.Infof("Detected stale FUSE mount for S3 volume %s at %s", volumeID, globalmountPath)
		} else {
			klog.Infof("Detected missing FUSE mount for S3 volume %s at %s", volumeID, globalmountPath)
		}

		// Heal in place: re-mount the volume in-process and re-attach consumers
		// without bouncing pods that can self-heal via mount propagation.
		ns.reconcileS3Mount(volumeID, globalmountPath, volumeContext)
	}

	klog.Infof("Stale/missing S3 mount cleanup completed")
}

// reconcileS3Mount heals a stale/missing S3 mount in place: re-mount the volume
// in-process (resuming from the LUKS VFS cache), then re-attach consumers —
// those with HostToContainer/Bidirectional propagation are left running, the
// rest are restarted. On re-mount failure it falls back to remove + restart all.
func (ns *NodeServer) reconcileS3Mount(volumeID, globalmountPath string, volumeContext map[string]string) {
	// Guard against the periodic checker racing with our own re-mount.
	ns.s3SyncMgr.markVolumeSetupInProgress(volumeID)
	defer ns.s3SyncMgr.markVolumeSetupComplete(volumeID)

	pvcNamespace, pvcName, pvName := ns.resolveVolumeRefs(volumeID)
	consumers := ns.podsUsingPVC(pvcNamespace, pvcName)
	fsGroup := fsGroupFromPods(consumers)

	// Detach the dead FUSE mount (if any) so a fresh mount can take the path.
	_ = exec.Command("umount", "-l", globalmountPath).Run()

	// Drop any stale in-memory manager, stopping its cache monitor first so it
	// doesn't keep evicting the cache dir the fresh mount is about to own.
	ns.s3SyncMgr.mutex.Lock()
	if old := ns.s3SyncMgr.mountManagers[volumeID]; old != nil {
		old.StopCacheMonitor()
	}
	delete(ns.s3SyncMgr.mountManagers, volumeID)
	ns.s3SyncMgr.mutex.Unlock()

	if err := os.MkdirAll(globalmountPath, 0777); err != nil {
		klog.Warningf("Volume %s: failed to ensure globalmount dir: %v", volumeID, err)
	}

	if err := ns.setupS3Sync(volumeID, globalmountPath, volumeContext, nil, fsGroup); err != nil {
		klog.Errorf("Volume %s: in-place re-mount failed (%v); falling back to remove + restart all consumers", volumeID, err)
		_ = os.RemoveAll(globalmountPath)
		ns.restartPodsWithStaleS3Mount(volumeID)
		return
	}
	klog.Infof("Volume %s: re-mounted in-process; re-attaching consumers", volumeID)

	for i := range consumers {
		pod := &consumers[i]
		// Re-point the consumer's stale bind at the fresh globalmount.
		rebound := ns.rebindConsumerMount(globalmountPath, string(pod.UID), pvName, fsGroup)

		if podSelfHealsViaPropagation(pod, pvcName) && ns.consumerMountHealthy(string(pod.UID), pvName) {
			klog.Infof("Pod %s/%s self-healed via mount propagation for volume %s; left running",
				pod.Namespace, pod.Name, volumeID)
			continue
		}
		// Re-bind failed: only a full re-publish (pod delete) can recover it.
		if !rebound {
			klog.Warningf("Pod %s/%s: re-bind failed for volume %s; deleting to force re-publish",
				pod.Namespace, pod.Name, volumeID)
			ns.deletePodByUID(string(pod.UID))
			continue
		}
		klog.Infof("Pod %s/%s cannot self-heal for volume %s; restarting to recover",
			pod.Namespace, pod.Name, volumeID)
		ns.recoverConsumerPod(pod)
	}
}

// rebindConsumerMount re-points a consumer's stale CSI bind mount at the freshly
// re-mounted globalmount (the host-side half of NodePublishVolume), so a
// restarted container lands on a live mount instead of the torn-down FUSE.
// Returns true when the host path now backs the fresh mount (including when the
// pod isn't published here).
func (ns *NodeServer) rebindConsumerMount(globalmountPath, podUID, pvName string, fsGroup *int64) bool {
	if pvName == "" {
		return false
	}
	targetPath := filepath.Join(resolveKubeletRoot(), "pods", podUID,
		"volumes", "kubernetes.io~csi", pvName, "mount")
	if _, err := os.Stat(filepath.Dir(targetPath)); err != nil {
		return true // not published on this node; nothing stale to re-point
	}

	_ = exec.Command("umount", "-l", targetPath).Run()
	if err := ns.bindMount(globalmountPath, targetPath, false, fsGroup); err != nil {
		klog.Warningf("Pod %s: failed to re-bind CSI mount %s to %s: %v",
			podUID, globalmountPath, targetPath, err)
		return false
	}
	return true
}

// recoverConsumerPod restarts the pod's containers in place so they re-bind the
// (already re-bound) host mount path. Falls back to pod deletion when containers
// won't restart (RestartPolicy=Never) or no processes are found.
func (ns *NodeServer) recoverConsumerPod(pod *corev1.Pod) {
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		if ns.restartPodContainers(pod) {
			return
		}
		klog.Warningf("Pod %s/%s: no container processes found to restart; falling back to pod deletion",
			pod.Namespace, pod.Name)
	}
	ns.deletePodByUID(string(pod.UID))
}

// restartPodContainers kills the pod's container processes (visible via
// hostPID) so kubelet restarts them with fresh volume binds, leaving the pod
// object — and its scheduling — untouched.
func (ns *NodeServer) restartPodContainers(pod *corev1.Pod) bool {
	pids := podContainerPIDs(string(pod.UID))
	if len(pids) == 0 {
		return false
	}
	klog.Infof("Pod %s/%s: killing %d container process(es) so they restart onto the repaired mount",
		pod.Namespace, pod.Name, len(pids))
	if ns.recorder != nil {
		ns.recorder.Event(pod, corev1.EventTypeWarning, "StaleS3MountRecovery",
			"Restarting containers in place: the S3-backed volume mount went stale and cannot self-heal via mount propagation")
	}
	killed := false
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			klog.Warningf("Pod %s/%s: failed to kill pid %d: %v", pod.Namespace, pod.Name, pid, err)
		} else {
			killed = true
		}
	}
	return killed
}

// podContainerPIDs returns the host PIDs of the pod's container processes,
// excluding the sandbox pause process so the pod sandbox survives.
func podContainerPIDs(podUID string) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cgroup, err := os.ReadFile("/proc/" + e.Name() + "/cgroup")
		if err != nil || !cgroupBelongsToPod(string(cgroup), podUID) {
			continue
		}
		comm, err := os.ReadFile("/proc/" + e.Name() + "/comm")
		if err == nil && strings.TrimSpace(string(comm)) == "pause" {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// cgroupBelongsToPod matches both cgroupfs (pod<uid>) and systemd
// (pod<uid_with_underscores>) cgroup path styles.
func cgroupBelongsToPod(cgroup, podUID string) bool {
	return strings.Contains(cgroup, "pod"+podUID) ||
		strings.Contains(cgroup, "pod"+strings.ReplaceAll(podUID, "-", "_"))
}

// consumerMountHealthy reports whether the re-mount reached a consumer pod's CSI
// mount path on the host (statfs not ENOTCONN/ESTALE), retrying briefly to let
// mount propagation settle. ENOENT means the pod isn't published here.
func (ns *NodeServer) consumerMountHealthy(podUID, pvName string) bool {
	if pvName == "" {
		return false
	}
	mountPath := filepath.Join(resolveKubeletRoot(), "pods", podUID,
		"volumes", "kubernetes.io~csi", pvName, "mount")

	for attempt := 0; attempt < 6; attempt++ {
		var st syscall.Statfs_t
		err := syscall.Statfs(mountPath, &st)
		if err == nil || errors.Is(err, syscall.ENOENT) {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// resolveVolumeRefs returns the bound PVC namespace/name and the PV name for a
// volume, or empty strings if it can't be resolved.
func (ns *NodeServer) resolveVolumeRefs(volumeID string) (pvcNamespace, pvcName, pvName string) {
	if ns.clientset == nil {
		return "", "", ""
	}
	pv, err := getPVByVolumeID(context.Background(), ns.clientset, volumeID)
	if err != nil || pv.Spec.ClaimRef == nil {
		klog.V(4).Infof("Volume %s: could not resolve PVC (consumer self-heal detection degraded): %v", volumeID, err)
		return "", "", ""
	}
	return pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name, pv.Name
}

// podsUsingPVC returns the pods on this node in the namespace that use the PVC.
func (ns *NodeServer) podsUsingPVC(namespace, pvcName string) []corev1.Pod {
	if ns.clientset == nil || namespace == "" || pvcName == "" {
		return nil
	}
	pods, err := ns.clientset.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + ns.driver.nodeID,
	})
	if err != nil {
		klog.Warningf("Failed to list pods in %s for PVC %s: %v", namespace, pvcName, err)
		return nil
	}
	var out []corev1.Pod
	for _, p := range pods.Items {
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == pvcName {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// fsGroupFromPods returns the first fsGroup declared by the pods, or nil — so an
// in-place re-mount preserves file ownership for non-root consumers.
func fsGroupFromPods(pods []corev1.Pod) *int64 {
	for i := range pods {
		if sc := pods[i].Spec.SecurityContext; sc != nil && sc.FSGroup != nil {
			return sc.FSGroup
		}
	}
	return nil
}

// podSelfHealsViaPropagation reports whether the pod mounts the PVC with
// HostToContainer/Bidirectional propagation in any (init)container — the only
// case where a running container observes an in-place re-mount on the host.
func podSelfHealsViaPropagation(pod *corev1.Pod, pvcName string) bool {
	if pvcName == "" {
		return false
	}
	volName := ""
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == pvcName {
			volName = v.Name
			break
		}
	}
	if volName == "" {
		return false
	}

	hasPropagation := func(mounts []corev1.VolumeMount) bool {
		for _, m := range mounts {
			if m.Name != volName || m.MountPropagation == nil {
				continue
			}
			if *m.MountPropagation == corev1.MountPropagationHostToContainer ||
				*m.MountPropagation == corev1.MountPropagationBidirectional {
				return true
			}
		}
		return false
	}

	for i := range pod.Spec.Containers {
		if hasPropagation(pod.Spec.Containers[i].VolumeMounts) {
			return true
		}
	}
	for i := range pod.Spec.InitContainers {
		if hasPropagation(pod.Spec.InitContainers[i].VolumeMounts) {
			return true
		}
	}
	return false
}

// restartPodsWithStaleS3Mount finds pods with stale mounts for this volume and restarts them
func (ns *NodeServer) restartPodsWithStaleS3Mount(volumeID string) {
	podsDir := resolveKubeletRoot() + "/pods"

	entries, err := os.ReadDir(podsDir)
	if err != nil {
		klog.Warningf("Failed to read pods directory %s: %v", podsDir, err)
		return
	}

	for _, podEntry := range entries {
		if !podEntry.IsDir() {
			continue
		}

		podUID := podEntry.Name()
		volumesDir := filepath.Join(podsDir, podUID, "volumes", "kubernetes.io~csi")
		volEntries, err := os.ReadDir(volumesDir)
		if err != nil {
			continue // Pod may not have CSI volumes
		}

		for _, volEntry := range volEntries {
			if !volEntry.IsDir() {
				continue
			}

			// Check if this volume matches by reading vol_data.json
			volDataPath := filepath.Join(volumesDir, volEntry.Name(), "vol_data.json")
			data, err := os.ReadFile(volDataPath)
			if err != nil {
				continue
			}

			var volData map[string]interface{}
			if err := json.Unmarshal(data, &volData); err != nil {
				continue
			}

			volHandle, ok := volData["volumeHandle"].(string)
			if !ok || volHandle != volumeID {
				continue
			}

			mountPath := filepath.Join(volumesDir, volEntry.Name(), "mount")

			// Check if mount path exists
			_, statErr := os.Lstat(mountPath)
			if os.IsNotExist(statErr) {
				continue
			}

			klog.Infof("Found pod %s using S3 volume %s, cleaning up mount and triggering restart", podUID, volumeID)

			// Unmount the pod's bind mount (may be stale or pointing to old staging)
			umountCmd := exec.Command("umount", "-l", mountPath)
			if err := umountCmd.Run(); err != nil {
				klog.V(4).Infof("umount -l for pod mount %s: %v (may already be unmounted)", mountPath, err)
			}

			// Delete the pod to trigger restart (if managed by a controller like Deployment)
			ns.deletePodByUID(podUID)
		}
	}
}

// deletePodByUID finds and deletes a pod by its UID
func (ns *NodeServer) deletePodByUID(podUID string) {
	if ns.clientset == nil {
		klog.Warningf("Cannot delete pod %s: no kubernetes client", podUID)
		return
	}

	ctx := context.Background()

	// List all pods to find the one with this UID
	pods, err := ns.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Warningf("Failed to list pods to find UID %s: %v", podUID, err)
		return
	}

	for _, pod := range pods.Items {
		if string(pod.UID) == podUID {
			// Skip pods that are already in a terminal phase — they completed normally
			// and should not be force-deleted (this would confuse StatefulSet/Job controllers
			// and could prevent the backup pod from ever being rescheduled).
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				klog.Infof("Pod %s/%s (UID: %s) is in terminal phase %s, skipping delete",
					pod.Namespace, pod.Name, podUID, pod.Status.Phase)
				return
			}
			// Skip pods already being deleted (DeletionTimestamp set) — kubelet is
			// handling teardown; no need to race with it.
			if pod.DeletionTimestamp != nil {
				klog.Infof("Pod %s/%s (UID: %s) is already being deleted, skipping",
					pod.Namespace, pod.Name, podUID)
				return
			}
			klog.Infof("Deleting pod %s/%s (UID: %s) to recover from stale S3 mount", pod.Namespace, pod.Name, podUID)
			if ns.recorder != nil {
				ns.recorder.Event(&pod, corev1.EventTypeWarning, "StaleS3MountRecovery",
					"Deleting pod: its S3-backed volume mount went stale and cannot self-heal via mount propagation")
			}
			err := ns.clientset.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
			if err != nil {
				klog.Errorf("Failed to delete pod %s/%s: %v", pod.Namespace, pod.Name, err)
			} else {
				klog.Infof("Successfully deleted pod %s/%s, it will be recreated by its controller", pod.Namespace, pod.Name)
			}
			return
		}
	}

	klog.Warningf("Could not find pod with UID %s to delete", podUID)
}

// isFUSEMountPoint checks if the path has a FUSE mount by reading /proc/mounts.
func (ns *NodeServer) isFUSEMountPoint(path string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		klog.Warningf("Failed to read /proc/mounts: %v", err)
		return false
	}

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[1] == path {
			// Check if it's a FUSE mount
			if strings.Contains(fields[2], "fuse") || strings.Contains(fields[0], "rclone") {
				return true
			}
		}
	}
	return false
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

// cleanupOrphanedVFSCacheDirs removes VFS cache directories for volumes whose
// PVs have been deleted. This handles the case where a PV is deleted while the
// CSI driver pod is down, leaving orphaned cache directories.
func (ns *NodeServer) cleanupOrphanedVFSCacheDirs() {
	if ns.clientset == nil {
		klog.Warning("Kubernetes client not available, skipping orphaned VFS cache cleanup")
		return
	}

	nameMap := rclone.LoadVFSNameMap()
	if len(nameMap) == 0 {
		klog.V(4).Infof("No persisted VFS name mappings, skipping orphan cleanup")
		return
	}

	klog.Infof("Checking %d persisted VFS name mappings for orphaned cache dirs", len(nameMap))

	ctx := context.Background()
	activeVolumeIDs := make(map[string]bool)

	for volumeID := range nameMap {
		_, err := ns.clientset.CoreV1().PersistentVolumes().Get(ctx, volumeID, metav1.GetOptions{})
		if err == nil {
			activeVolumeIDs[volumeID] = true
		} else if !k8serrors.IsNotFound(err) {
			// API error — treat as active to be safe
			klog.Warningf("Error checking PV %s: %v, treating as active", volumeID, err)
			activeVolumeIDs[volumeID] = true
		}
	}

	rclone.CleanupOrphanedVFSCacheDirs(activeVolumeIDs)
	klog.Infof("Orphaned VFS cache cleanup completed")
}

