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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/lukscryptwalker-csi/pkg/rclone"
	"github.com/lukscryptwalker-csi/pkg/secrets"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	// Directory-metadata caching (mount options). Raise for metadata-heavy
	// workloads on large directories.
	DirCacheTimeParam = "rclone-dir-cache-time" // e.g., "5m", "1h"
	AttrTimeoutParam  = "rclone-attr-timeout"   // e.g., "5m", "1h"
	// Per-volume budget: parallel chunk download streams. Cap low (e.g. "2")
	// for heavy volumes so one tenant can't monopolize memory and bandwidth.
	ChunkStreamsParam = "rclone-vfs-read-chunk-streams"
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
	// volumeLocks serializes mount/unmount work per volume: markVolumeSetupInProgress
	// is advisory (only the checker consults it), so two mounts could otherwise
	// interleave and register two VFSes under one name.
	volumeLocks sync.Map // volumeID → *sync.Mutex
}

// lockVolume serializes mount lifecycle work for one volume. Take it in CSI
// handlers and reconcile — never in setupS3Sync/cleanupS3Sync, which run under it.
func (sm *S3SyncManager) lockVolume(volumeID string) func() {
	v, _ := sm.volumeLocks.LoadOrStore(volumeID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
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

// startBackgroundDrain claims the volume's single drain slot. It returns false
// when a drain is already running: a second one would mount a second VFS under
// the same name, which makes the upload queue permanently unobservable.
func (sm *S3SyncManager) startBackgroundDrain(volumeID string) bool {
	sm.drainMu.Lock()
	defer sm.drainMu.Unlock()
	if _, running := sm.backgroundDrains[volumeID]; running {
		return false
	}
	sm.backgroundDrains[volumeID] = make(chan struct{})
	rclone.SaveDrainPending(volumeID)
	return true
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

// finishBackgroundDrain releases the drain slot. The persistent marker is
// cleared only when the drain actually completed: clearing it on failure
// destroyed the one record that this node still held the volume's only copy,
// so nothing afterwards — not the checker, not the next driver start — could
// tell that its cache was more than a stale read cache.
func (sm *S3SyncManager) finishBackgroundDrain(volumeID string, drained bool) {
	sm.drainMu.Lock()
	defer sm.drainMu.Unlock()
	if done, ok := sm.backgroundDrains[volumeID]; ok {
		close(done)
		delete(sm.backgroundDrains, volumeID)
	}
	if !drained {
		sm.pendingDrains[volumeID] = true
		rclone.SaveDrainPending(volumeID)
		return
	}
	delete(sm.pendingDrains, volumeID)
	rclone.ClearDrainPending(volumeID)
}

// waitForBackgroundDrain waits for the volume's background drain to finish.
// Returns false on timeout — callers in CSI RPCs must fail fast and retryable
// rather than blocking past kubelet's deadline.
func (sm *S3SyncManager) waitForBackgroundDrain(volumeID string, timeout time.Duration) bool {
	sm.drainMu.Lock()
	done, ok := sm.backgroundDrains[volumeID]
	sm.drainMu.Unlock()
	if !ok {
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		klog.Warningf("Volume %s: timed out waiting for background drain", volumeID)
		return false
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
	// If a background drain is still running (previous pod terminated with
	// uploads in progress), wait briefly for it to finish before remounting.
	// Never block past kubelet's CSI deadline (~2min): return a retryable
	// error and let the drain finish in the background.
	if ns.s3SyncMgr.isBackgroundDraining(volumeID) {
		klog.Infof("Volume %s: waiting for background drain before remounting", volumeID)
		if !ns.s3SyncMgr.waitForBackgroundDrain(volumeID, 45*time.Second) {
			return fmt.Errorf("volume %s: background drain still in progress, retry later", volumeID)
		}
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

	if err := ns.mountS3Volume(volumeID, stagingPath, volumeContext, fsGroup); err != nil {
		return err
	}
	ns.mountedAt.Store(volumeID, time.Now())
	return nil
}

// reportMountLifetime logs how long the mount that just vanished had been up.
// A short and repeatable lifetime points at a teardown racing our own mount; a
// scattered one points at an external unmounter.
func (ns *NodeServer) reportMountLifetime(volumeID string) {
	v, ok := ns.mountedAt.LoadAndDelete(volumeID)
	if !ok {
		return
	}
	klog.Warningf("Volume %s: the mount that just went away had been up for %s",
		volumeID, time.Since(v.(time.Time)).Round(time.Second))
}

// mountS3Volume builds the rclone mount for a volume and registers its manager.
// Split out of setupS3Sync so the stranded-drain path can re-mount without
// tripping the drain gates it holds itself.
func (ns *NodeServer) mountS3Volume(volumeID, stagingPath string, volumeContext map[string]string, fsGroup *int64) error {
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
	mountMgr, err := rclone.NewMountManager(s3Config, volumeID, stagingPath, vfsConfig, s3PathPrefix, passphrase, fsGroup)
	if err != nil {
		return fmt.Errorf("failed to create rclone mount manager: %v", err)
	}

	// Mount the encrypted S3 remote. %w, not %v: callers distinguish a mount
	// ripped out by a stale finalizer (retry now) from a real failure.
	if err := mountMgr.Mount(); err != nil {
		return fmt.Errorf("failed to mount S3 volume: %w", err)
	}

	// Store mount manager, stopping the monitors of any manager it replaces:
	// those keep polling a VFS name this mount no longer uses and keep evicting
	// a cache dir the fresh mount now owns.
	ns.s3SyncMgr.mutex.Lock()
	if old := ns.s3SyncMgr.mountManagers[volumeID]; old != nil && old != mountMgr {
		old.StopCacheMonitor()
	}
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

	if chunkStreams, exists := volumeContext[ChunkStreamsParam]; exists && chunkStreams != "" {
		config.ChunkStreams = chunkStreams
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

// ErrUnuploadedData means the node still holds writes that never reached S3.
// NodeUnstageVolume must fail on it: reporting the volume unstaged is what lets
// the CO start the consumer on another node, where the same S3 prefix is missing
// everything still queued here.
var ErrUnuploadedData = errors.New("volume still holds writes that have not reached S3")

// cleanupS3Sync unmounts an S3 volume. It returns ErrUnuploadedData while the
// only copy of any data is still local, in which case the caller must NOT report
// the volume as unstaged. stagingTargetPath may be empty for orphan reclaim of a
// deleted PV, which skips the unuploaded-data guard — that data is meant to go.
func (ns *NodeServer) cleanupS3Sync(volumeID, stagingTargetPath string) error {
	klog.Infof("Cleaning up S3 mount for volume %s", volumeID)

	// Refusing to unstage blocks pod teardown for as long as S3 stays
	// unreachable, so an operator who has decided to abandon the unuploaded
	// writes needs a way to say so that is deliberate and auditable. Gated on a
	// local check first: the common case is a clean volume, and that must not
	// cost two API calls on every teardown.
	if (ns.s3SyncMgr.isBackgroundDraining(volumeID) || rclone.HasUnuploadedData(volumeID)) &&
		ns.forceUnstageRequested(volumeID) {
		return ns.forceCleanupS3Sync(volumeID)
	}

	// Kubelet retried NodeUnstageVolume while a previous drain is still running.
	if ns.s3SyncMgr.isBackgroundDraining(volumeID) {
		klog.Infof("Volume %s: background drain still in progress", volumeID)
		return fmt.Errorf("%w: upload still draining on node %s", ErrUnuploadedData, ns.driver.nodeID)
	}

	ns.s3SyncMgr.mutex.Lock()
	mountMgr, exists := ns.s3SyncMgr.mountManagers[volumeID]
	ns.s3SyncMgr.mutex.Unlock()
	if !exists {
		// No manager: either never mounted here, or the driver restarted and lost
		// it while the mount stayed up. In the second case the cache dir is the
		// only remaining evidence, and tearing the staging mount down from here
		// would abandon it — resume the upload instead.
		if stagingTargetPath == "" || !rclone.HasUnuploadedData(volumeID) {
			return nil
		}
		ns.resumeStrandedDrain(volumeID, stagingTargetPath,
			"no mount manager for this volume (driver restarted while it was mounted)")
		return fmt.Errorf("%w: resuming upload from the VFS cache on node %s", ErrUnuploadedData, ns.driver.nodeID)
	}

	// Fast path, bounded and NOT under the manager-map lock: holding it through
	// a long drain would stall every other S3 volume on the node.
	if mountMgr.IsUploadQueueEmpty() {
		drained, err := mountMgr.UnmountWithin(rclone.ShortDrainWait)
		if err != nil {
			return err
		}
		ns.s3SyncMgr.mutex.Lock()
		delete(ns.s3SyncMgr.mountManagers, volumeID)
		ns.s3SyncMgr.mutex.Unlock()
		if !drained && stagingTargetPath != "" {
			// The queue looked empty but the cache disagreed. The mount is gone
			// now, so nothing is uploading: only a fresh mount re-queues those
			// items, and retrying the unmount would spin forever without one.
			ns.resumeStrandedDrain(volumeID, stagingTargetPath, "unmount completed without confirming the upload")
			return fmt.Errorf("%w: unconfirmed upload on node %s", ErrUnuploadedData, ns.driver.nodeID)
		}
		return nil
	}

	// Uploads in progress. Drain in the background so this handler returns
	// promptly, but return an error: kubelet retries NodeUnstageVolume and the
	// consumer cannot be rescheduled elsewhere until a retry finds the queue
	// empty. Waiting for S3 is the price of not serving an empty volume.
	ns.s3SyncMgr.startBackgroundDrain(volumeID)
	go func() {
		drained := false
		defer func() { ns.s3SyncMgr.finishBackgroundDrain(volumeID, drained) }()
		klog.Infof("Volume %s: background drain started", volumeID)
		ns.s3SyncMgr.mutex.Lock()
		mm := ns.s3SyncMgr.mountManagers[volumeID]
		ns.s3SyncMgr.mutex.Unlock()
		if mm == nil {
			return
		}
		var err error
		if drained, err = mm.Unmount(); err != nil {
			klog.Errorf("Volume %s: background drain unmount failed: %v", volumeID, err)
		}
		ns.s3SyncMgr.mutex.Lock()
		delete(ns.s3SyncMgr.mountManagers, volumeID)
		ns.s3SyncMgr.mutex.Unlock()
		if !drained {
			ns.reportUnuploadedData(volumeID, "background drain finished without uploading everything")
			return
		}
		klog.Infof("Volume %s: background drain complete", volumeID)
	}()

	return fmt.Errorf("%w: draining %s", ErrUnuploadedData, volumeID)
}

// ForceUnstageAnnotation, set on the PVC or the PV, tells the driver to unstage
// a volume even though this node still holds writes that never reached S3. It
// abandons that data. It exists because the alternative — refusing forever while
// S3 is unreachable — leaves the consumer stuck in Terminating with no recourse.
const ForceUnstageAnnotation = "lukscryptwalker.io/force-unstage"

// forceUnstageRequested reports whether an operator has explicitly accepted the
// loss of this volume's unuploaded writes.
func (ns *NodeServer) forceUnstageRequested(volumeID string) bool {
	if ns.clientset == nil {
		return false
	}
	ctx := context.Background()
	pv, err := getPVByVolumeID(ctx, ns.clientset, volumeID)
	if err != nil {
		return false
	}
	if pv.Annotations[ForceUnstageAnnotation] == "true" {
		return true
	}
	if pv.Spec.ClaimRef == nil {
		return false
	}
	pvc, err := ns.clientset.CoreV1().PersistentVolumeClaims(pv.Spec.ClaimRef.Namespace).
		Get(ctx, pv.Spec.ClaimRef.Name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	return pvc.Annotations[ForceUnstageAnnotation] == "true"
}

// forceCleanupS3Sync tears the mount down without waiting for the upload, and
// leaves the cache on disk: the annotation authorises giving up on the handover,
// not deleting the only copy of the data.
func (ns *NodeServer) forceCleanupS3Sync(volumeID string) error {
	klog.Errorf("Volume %s: %s=true — unstaging without confirming the upload. Any writes still in %s on node %s "+
		"will NOT appear when this volume is mounted elsewhere; the cache is kept for manual recovery",
		volumeID, ForceUnstageAnnotation, rclone.VFSCacheBasePath, ns.driver.nodeID)
	if ns.recorder != nil {
		ref := ns.pvcRef(volumeID)
		if ref == nil {
			ref = ns.nodeRef()
		}
		ns.recorder.Eventf(ref, corev1.EventTypeWarning, "ForcedUnstageWithUnuploadedData",
			"Volume %s: unstaged from node %s by operator request while writes were still unuploaded. Those writes "+
				"remain only in that node's local cache.", volumeID, ns.driver.nodeID)
	}

	ns.s3SyncMgr.mutex.Lock()
	mountMgr := ns.s3SyncMgr.mountManagers[volumeID]
	delete(ns.s3SyncMgr.mountManagers, volumeID)
	ns.s3SyncMgr.mutex.Unlock()
	if mountMgr == nil {
		return nil
	}
	if _, err := mountMgr.UnmountWithin(rclone.ShortDrainWait); err != nil {
		return err
	}
	return nil
}

// resumeStrandedDrain re-mounts a volume whose VFS cache still holds unuploaded
// writes but whose mount manager is gone, so rclone's cache reload finishes the
// upload. Without this the only recovery was kubelet happening to stage the same
// volume on this node again, which never happens once the consumer moves away.
func (ns *NodeServer) resumeStrandedDrain(volumeID, stagingTargetPath, reason string) {
	if stagingTargetPath == "" {
		return
	}
	volumeContext := ns.getS3VolumeContext(context.Background(), volumeID)
	if volumeContext == nil {
		klog.Errorf("Volume %s: holds unuploaded writes but its PV is unreadable, so the upload cannot be resumed; "+
			"the data stays in %s on node %s", volumeID, rclone.VFSCacheBasePath, ns.driver.nodeID)
		return
	}

	// Claims the drain slot, so kubelet's NodeUnstageVolume retries and the
	// checker tick do not each start their own mount of the same volume.
	if !ns.s3SyncMgr.startBackgroundDrain(volumeID) {
		return
	}
	ns.reportUnuploadedData(volumeID, reason)

	go func() {
		drained := false
		defer func() { ns.s3SyncMgr.finishBackgroundDrain(volumeID, drained) }()

		klog.Infof("Volume %s: re-mounting at %s to upload writes stranded in the VFS cache",
			volumeID, stagingTargetPath)
		// mountS3Volume, not setupS3Sync: the drain gates there would deadlock
		// against the slot this goroutine already holds.
		if err := ns.mountS3Volume(volumeID, stagingTargetPath, volumeContext, nil); err != nil {
			klog.Errorf("Volume %s: could not re-mount to resume the upload: %v", volumeID, err)
			return
		}

		ns.s3SyncMgr.mutex.Lock()
		mm := ns.s3SyncMgr.mountManagers[volumeID]
		ns.s3SyncMgr.mutex.Unlock()
		if mm == nil {
			return
		}
		var err error
		if drained, err = mm.Unmount(); err != nil {
			klog.Errorf("Volume %s: resumed drain unmount failed: %v", volumeID, err)
		}
		ns.s3SyncMgr.mutex.Lock()
		delete(ns.s3SyncMgr.mountManagers, volumeID)
		ns.s3SyncMgr.mutex.Unlock()
		if drained {
			klog.Infof("Volume %s: stranded writes uploaded, the volume is now safe to mount elsewhere", volumeID)
		} else {
			ns.reportUnuploadedData(volumeID, "resumed drain did not complete")
		}
		// Only our own scratch mountpoint; a kubelet staging path is kubelet's.
		if strings.HasPrefix(stagingTargetPath, drainMountBase+"/") {
			removeAllBounded(stagingTargetPath, 30*time.Second)
		}
	}()
}

// reportUnuploadedData makes stranded data visible outside this node's logs.
// The cache is node-local and the PV carries no record of it, so an event on the
// PVC is the only place an operator looking at the workload can see which node
// still holds the data.
func (ns *NodeServer) reportUnuploadedData(volumeID, reason string) {
	klog.Errorf("Volume %s: %s — node %s holds writes that never reached S3. The volume must not be mounted "+
		"on another node until they upload; the cache is at %s", volumeID, reason, ns.driver.nodeID, rclone.VFSCacheBasePath)
	if ns.recorder == nil {
		return
	}
	ref := ns.pvcRef(volumeID)
	if ref == nil {
		ref = ns.nodeRef()
	}
	ns.recorder.Eventf(ref, corev1.EventTypeWarning, "UnuploadedData",
		"Volume %s: %s. Node %s still holds writes that have not reached S3; mounting this volume elsewhere "+
			"would serve an incomplete view, so unstaging is being refused until the upload completes.",
		volumeID, reason, ns.driver.nodeID)
}

// pvcRef returns an event target on the volume's claim, or nil when it cannot
// be resolved.
func (ns *NodeServer) pvcRef(volumeID string) *corev1.ObjectReference {
	if ns.clientset == nil {
		return nil
	}
	pv, err := getPVByVolumeID(context.Background(), ns.clientset, volumeID)
	if err != nil || pv.Spec.ClaimRef == nil {
		return nil
	}
	return pv.Spec.ClaimRef
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

// kubeletRootCache memoizes the resolved kubelet root: the value cannot change
// while we run, and it is read on every checker tick and CSI call.
var kubeletRootCache atomic.Value // string

// resolveKubeletRoot resolves /var/lib/kubelet to its real path on the host.
// On microk8s, /var/lib/kubelet is a symlink to /var/snap/microk8s/common/var/lib/kubelet.
// We resolve in the host namespace using nsenter so the path matches what kubelet
// passes in CSI requests and what rclone uses for FUSE mounts.
//
// Resolved once and cached, with a hard timeout: an unbounded nsenter here
// froze the whole checker when the host stalled.
func resolveKubeletRoot() string {
	if v, ok := kubeletRootCache.Load().(string); ok && v != "" {
		return v
	}

	resolved := DefaultKubeletRoot
	out, err := runCmdBoundedOutput(10*time.Second, "nsenter", "-t", "1", "-m", "-u", "readlink", "-f", DefaultKubeletRoot)
	if err != nil {
		klog.Warningf("Could not resolve kubelet root (%v); using %s", err, DefaultKubeletRoot)
	} else if trimmed := strings.TrimSpace(out); trimmed != "" {
		resolved = trimmed
	}

	kubeletRootCache.Store(resolved)
	return resolved
}

// cleanupStaleS3Mounts cleans up stale or missing S3/FUSE mounts that may remain after
// an unclean shutdown (e.g., OOM kill). For S3 volumes, it attempts to restore
// the mount instead of just unmounting to keep existing pods working.
func (ns *NodeServer) cleanupStaleS3Mounts() {
	// During a node-wide I/O stall every mount looks dead; recovering them
	// mid-stall kills healthy consumers and adds churn. Wait the wave out.
	if pct, stalled := nodeIOStalled(); stalled {
		klog.Warningf("Node I/O is stalling (PSI full avg10=%.0f%%); deferring stale-mount recovery this tick", pct)
		return
	}

	// Every decision below rests on the host mount table; unreadable makes
	// "no mount" and "cannot tell" indistinguishable, so skip the tick.
	if !rclone.HostMountsKnown() {
		klog.Error("Host mount table is unreadable (a wedged umount holds the kernel mount lock); skipping this " +
			"stale-mount tick — mount state cannot be distinguished from absence")
		return
	}

	// One registry snapshot per tick: the authority on which mounts still have
	// a live fs behind them (see vfsRegistryMissing).
	vfsNames, vfsNamesKnown := rclone.RegisteredVFSNames()

	kubeletRoot := resolveKubeletRoot()
	csiPluginPath := kubeletRoot + "/plugins/kubernetes.io/csi/" + DriverName
	klog.Infof("Checking for stale/missing S3 mounts in %s", csiPluginPath)

	// A missing plugin path means this checker can never repair anything —
	// usually the kubelet root resolved to a path the container does not
	// expose (see node.kubeletDir). Warn: silently returning here makes a
	// completely blind checker look identical to a working one.
	if _, err := os.Stat(csiPluginPath); os.IsNotExist(err) {
		klog.Warningf("CSI plugin path %s does not exist — stale-mount recovery is INACTIVE on this node; "+
			"check that node.kubeletDir matches `readlink -f /var/lib/kubelet` on the host", csiPluginPath)
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
		// surfaces as ENOTCONN/ESTALE. Bounded: a wedged FUSE (stuck serve
		// loop, open fd) blocks statfs in D-state forever — that must mark the
		// mount stale, not freeze this checker.
		statErr, timedOut := ns.statfsBounded(globalmountPath, 5*time.Second)
		if !timedOut && errors.Is(statErr, syscall.ENOENT) {
			// globalmount directory does not exist yet - nothing to clean up
			continue
		}

		// Stale: statfs timeout (wedged FUSE), ENOTCONN/ESTALE (dead daemon),
		// or EIO (aborted connection / zombie VFS). Other errnos (e.g. EINTR)
		// don't justify a disruptive reconcile — log and leave the mount alone.
		isStaleFUSE := timedOut || isMountDeadErr(statErr)
		if timedOut {
			klog.Warningf("S3 mount %s: statfs blocked >5s (wedged FUSE); treating as stale", globalmountPath)
		} else if statErr != nil && !isStaleFUSE {
			klog.Warningf("S3 mount %s: unexpected statfs error (not reconciling): %v", globalmountPath, statErr)
		}

		// Check if mount is missing (directory exists but no FUSE mount)
		// This happens when the stale mount was already cleaned up but pods still need it
		isMissingMount := statErr == nil && !ns.isFUSEMountPoint(globalmountPath)

		// rclone's registry is the authority on a zombie: statfs is answered by
		// the FUSE layer and listings by the dir cache, so both keep passing
		// after the fs behind them is gone. Ask it before the read probe, which
		// only proves the mount dead if it stumbles onto an uncached file.
		if !isStaleFUSE && statErr == nil && !isMissingMount &&
			ns.vfsRegistryMissing(volumeDir, vfsNames, vfsNamesKnown) {
			klog.Warningf("S3 mount %s has no VFS registered in rclone (zombie: the FUSE mount answers, the fs behind "+
				"it is shut down); reconciling", globalmountPath)
			isStaleFUSE = true
		}

		// statfs is FUSE-local, so a healthy-looking mount can be a cancelled-VFS
		// zombie (real ops return EIO); probe it and reconcile if unresponsive.
		if !isStaleFUSE && statErr == nil && !isMissingMount && !ns.mountVFSResponsive(globalmountPath) {
			klog.Warningf("S3 mount %s passes statfs but fails directory reads (cancelled-VFS zombie); reconciling", globalmountPath)
			isStaleFUSE = true
		}

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
		// Skip volumes whose drain goroutine is still live — it owns the cache.
		// A merely *pending* drain is not skipped: nothing is uploading it, and
		// re-mounting is how the cache gets uploaded, so skipping it here is what
		// left unuploaded data sitting on the node with no path back to S3.
		if ns.s3SyncMgr.isBackgroundDraining(volumeID) {
			klog.V(4).Infof("Volume %s has an active drain, skipping stale detection", volumeID)
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
			// "Missing" is also what a NORMAL kubelet unstage looks like: the
			// last consumer went away and kubelet tore the staging mount down.
			// Re-mounting then fights kubelet — it unstages again, we re-mount
			// again — and each cycle can escalate to deleting a consumer that
			// was only restarting. With no consumer on this node there is
			// nothing to heal, so leave it alone.
			pvcNS, pvcName, _ := ns.resolveVolumeRefs(volumeID)
			if len(ns.podsUsingPVC(pvcNS, pvcName)) == 0 {
				klog.V(4).Infof("Volume %s is unmounted and has no consumer on this node — leaving it to kubelet",
					volumeID)
				continue
			}
			klog.Infof("Detected missing FUSE mount for S3 volume %s at %s", volumeID, globalmountPath)
		}
		ns.reportMountLifetime(volumeID)

		// Heal in place: re-mount the volume in-process and re-attach consumers
		// without bouncing pods that can self-heal via mount propagation.
		ns.reconcileS3Mount(volumeID, globalmountPath, volumeContext)
	}

	klog.Infof("Stale/missing S3 mount cleanup completed")
}

// drainMountBase is a driver-private, host-propagated directory used to mount a
// volume just long enough to upload writes stranded in its VFS cache. Not a
// kubelet path: kubelet has finished with these volumes, and the only thing left
// to do with them on this node is finish their upload.
const drainMountBase = "/var/lib/lukscrypt-cache/drain"

// strandedRetryInterval floors how often one volume's stranded-upload recovery
// is re-attempted, for attempts that fail before they can hold the drain slot.
const strandedRetryInterval = 5 * time.Minute

// recoverStrandedVolumes uploads writes left in the VFS cache of volumes kubelet
// no longer stages here. Nothing else can reach them: CSI calls only arrive for
// volumes kubelet still tracks, so once the consumer moves to another node the
// cached writes have no path back to S3 and the volume reads empty everywhere.
func (ns *NodeServer) recoverStrandedVolumes() {
	stranded := rclone.VolumesWithUnuploadedData()
	if len(stranded) == 0 {
		return
	}

	staged, known := ns.stagedVolumeIDs()
	if !known {
		klog.Warningf("Cannot tell which volumes kubelet still stages here; deferring recovery of %d volume(s) "+
			"holding unuploaded writes", len(stranded))
		return
	}

	for _, volumeID := range stranded {
		// Kubelet still stages it here: a live mount owns the cache, and the
		// stale-mount checker owns its repair. Mounting a second VFS under the
		// same name is the one thing that makes the queue permanently unobservable.
		if staged[volumeID] || ns.s3SyncMgr.isVolumeSetupInProgress(volumeID) ||
			ns.s3SyncMgr.isBackgroundDraining(volumeID) {
			continue
		}
		ns.s3SyncMgr.mutex.RLock()
		_, mounted := ns.s3SyncMgr.mountManagers[volumeID]
		ns.s3SyncMgr.mutex.RUnlock()
		if mounted {
			continue
		}
		// An attempt that fails before it can mount frees the drain slot at once;
		// without a floor this would retry on every 30s tick forever.
		if last, ok := ns.strandedRetry.Load(volumeID); ok && time.Since(last.(time.Time)) < strandedRetryInterval {
			continue
		}
		// The operator has already accepted losing this data; do not keep
		// re-mounting the volume to chase an upload they gave up on.
		if ns.forceUnstageRequested(volumeID) {
			continue
		}
		ns.strandedRetry.Store(volumeID, time.Now())

		ns.resumeStrandedDrain(volumeID, filepath.Join(drainMountBase, volumeID),
			"kubelet no longer stages this volume here, but its cache is not empty")
	}
}

// stagedVolumeIDs returns the volumes kubelet still has a staging directory for
// on this node. known is false when the answer could not be obtained: "unknown"
// must never be read as "none", or a private drain VFS gets mounted alongside a
// live one and every vfs/* RPC for that volume turns ambiguous.
func (ns *NodeServer) stagedVolumeIDs() (staged map[string]bool, known bool) {
	csiPluginPath := resolveKubeletRoot() + "/plugins/kubernetes.io/csi/" + DriverName
	entries, err := os.ReadDir(csiPluginPath)
	if err != nil {
		if os.IsNotExist(err) {
			// kubelet stages nothing here at all; that is a real answer.
			return map[string]bool{}, true
		}
		klog.Warningf("Could not list staged volumes in %s: %v", csiPluginPath, err)
		return nil, false
	}
	staged = make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if id := ns.getVolumeIDFromVolData(filepath.Join(csiPluginPath, e.Name())); id != "" {
			staged[id] = true
		}
	}
	return staged, true
}

// reconcileS3Mount heals a stale/missing S3 mount in place: re-mount the volume
// in-process (resuming from the LUKS VFS cache), then re-attach consumers —
// those with HostToContainer/Bidirectional propagation are left running, the
// rest are restarted. On re-mount failure it falls back to remove + restart all.
func (ns *NodeServer) reconcileS3Mount(volumeID, globalmountPath string, volumeContext map[string]string) {
	// Serialize against CSI handlers: the setup-in-progress flag is advisory.
	defer ns.s3SyncMgr.lockVolume(volumeID)()

	// Guard against the periodic checker racing with our own re-mount.
	ns.s3SyncMgr.markVolumeSetupInProgress(volumeID)
	defer ns.s3SyncMgr.markVolumeSetupComplete(volumeID)

	pvcNamespace, pvcName, pvName := ns.resolveVolumeRefs(volumeID)
	consumers := ns.podsUsingPVC(pvcNamespace, pvcName)
	fsGroup := fsGroupFromPods(consumers)

	// Drop any stale in-memory manager, stopping its cache monitor first so it
	// doesn't keep evicting the cache dir the fresh mount is about to own.
	ns.s3SyncMgr.mutex.Lock()
	if old := ns.s3SyncMgr.mountManagers[volumeID]; old != nil {
		old.StopCacheMonitor()
	}
	delete(ns.s3SyncMgr.mountManagers, volumeID)
	ns.s3SyncMgr.mutex.Unlock()

	// Shut down the old librclone session (VFS.Shutdown) before the kernel
	// detach: a bare umount -l leaves the old VFS alive in rclone's registry,
	// writing the shared cache dir and able to unmount the fresh mount later.
	hadSession := rclone.UnmountDead(globalmountPath)
	// Abort the kernel FUSE connection: a wedged serve loop leaves stat/open
	// callers in uninterruptible sleep, and only the abort releases them.
	abortFUSEConnection(globalmountPath)
	_ = runCmdBounded(30*time.Second, "umount", "-l", globalmountPath)

	if err := mkdirAllBounded(globalmountPath, 0777, 30*time.Second); err != nil {
		klog.Warningf("Volume %s: failed to ensure globalmount dir: %v", volumeID, err)
	}

	// An old session's finalizer fires seconds after its serve loop exits and
	// unmounts whatever rclone mount it finds at the path. Retry here rather
	// than next tick: each session rips out at most one successor, so this
	// converges, whereas one attempt per tick just mounts the next victim.
	mounted := false
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if hadSession && attempt == 0 {
			time.Sleep(2 * time.Second)
		}
		err := ns.setupS3Sync(volumeID, globalmountPath, volumeContext, nil, fsGroup)
		if err == nil {
			mounted = true
			break
		}
		lastErr = err
		ns.s3SyncMgr.mutex.Lock()
		if old := ns.s3SyncMgr.mountManagers[volumeID]; old != nil {
			old.StopCacheMonitor()
		}
		delete(ns.s3SyncMgr.mountManagers, volumeID)
		ns.s3SyncMgr.mutex.Unlock()
		hadSession = rclone.UnmountDead(globalmountPath)

		// Only a rip-out is worth retrying now; any other failure would burn the
		// checker budget four times over. Next tick retries with backoff.
		if !errors.Is(err, rclone.ErrMountRippedOut) {
			klog.Errorf("Volume %s: in-place re-mount failed: %v", volumeID, err)
			break
		}
		klog.Warningf("Volume %s: attempt %d was unmounted by a previous session's finalizer; that session has "+
			"now spent its unmount, retrying immediately", volumeID, attempt+1)
	}
	if !mounted {
		// Last resort, not the response to one failed mount: rate-limited like
		// every other destructive recovery.
		if lastErr != nil && ns.consumerRestartAllowed(volumeID) {
			klog.Errorf("Volume %s: in-place re-mount keeps failing (%v); falling back to remove + restart all consumers",
				volumeID, lastErr)
			removeAllBounded(globalmountPath, 60*time.Second)
			ns.restartPodsWithStaleS3Mount(volumeID)
			return
		}
		klog.Errorf("Volume %s: fresh mount did not survive; leaving consumers alone, next checker tick retries", volumeID)
		return
	}
	klog.Infof("Volume %s: re-mounted in-process; re-attaching consumers", volumeID)

	// Destructive recovery (container kills / pod deletes) at most once per
	// cooldown per volume: a reconcile loop must degrade to log noise, never
	// to repeatedly killing consumers.
	restartBudget := ns.consumerRestartAllowed(volumeID)

	for i := range consumers {
		pod := &consumers[i]

		// Don't re-bind onto a dying pod; drop its stale bind so kubelet can
		// finish teardown, then force-delete it if it's wedged.
		if pod.DeletionTimestamp != nil {
			ns.unbindTerminatingConsumer(string(pod.UID), pvName)
			ns.deletePodByUID(string(pod.UID))
			continue
		}

		// Re-point the consumer's stale bind at the fresh globalmount.
		rebound := ns.rebindConsumerMount(globalmountPath, string(pod.UID), pvName, fsGroup)

		if podSelfHealsViaPropagation(pod, pvcName) && ns.consumerMountHealthy(string(pod.UID), pvName, globalmountPath) {
			klog.Infof("Pod %s/%s self-healed via mount propagation for volume %s; left running",
				pod.Namespace, pod.Name, volumeID)
			continue
		}
		if !restartBudget {
			klog.Warningf("Pod %s/%s: needs restart for volume %s but consumers were restarted recently; skipping this cycle",
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

// consumerRestartCooldown bounds how often reconcile may destructively recover
// a volume's consumers.
const consumerRestartCooldown = 5 * time.Minute

// consumerRestartAllowed reports whether destructive consumer recovery is
// allowed for the volume, stamping the cooldown when it is.
func (ns *NodeServer) consumerRestartAllowed(volumeID string) bool {
	// Killing a consumer while kubelet has no driver to call is a one-way
	// door: nothing can re-stage the volume, so the pod never comes back and a
	// degraded-but-running workload becomes a hard outage.
	if !ns.registrationHealthy() {
		klog.Warningf("Volume %s: leaving consumers alone — this driver is not registered with kubelet, so a "+
			"restarted pod could not re-stage the volume until the node-driver-registrar re-registers it",
			volumeID)
		return false
	}
	if t, ok := ns.consumerRestartTimes.Load(volumeID); ok {
		if since := time.Since(t.(time.Time)); since < consumerRestartCooldown {
			klog.Warningf("Volume %s: consumers were destructively recovered %s ago (cooldown %s)",
				volumeID, since.Round(time.Second), consumerRestartCooldown)
			return false
		}
	}
	ns.consumerRestartTimes.Store(volumeID, time.Now())
	return true
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

	// The path must be fully clear first. Binding over a mount we failed to
	// remove stacks the fresh one on the stale one, and a container started
	// before the rebind keeps talking to the stale one underneath.
	if err := unmountHostStack(targetPath); err != nil {
		klog.Warningf("Pod %s: not re-binding %s, its stale mount is still there: %v", podUID, targetPath, err)
		return false
	}
	if err := ns.bindMount(globalmountPath, targetPath, false, fsGroup); err != nil {
		klog.Warningf("Pod %s: failed to re-bind CSI mount %s to %s: %v",
			podUID, globalmountPath, targetPath, err)
		return false
	}
	return true
}

// consumerBind is one pod's bind of one of our volumes, as the host sees it.
type consumerBind struct {
	path   string
	podUID string
	pvName string
	depth  int    // mounts stacked here; >1 means a re-bind left the old one
	dev    string // device of the topmost mount, the one the host resolves
}

// consumerBinds returns every consumer bind of one of our volumes and the set
// of devices the host still resolves, from the mount table alone — no API
// calls, so a healthy node costs nothing.
func consumerBinds() ([]consumerBind, map[string]bool, bool) {
	stacks, known := rclone.HostMountStacksOK()
	if !known {
		return nil, nil, false
	}
	return parseConsumerBinds(stacks, resolveKubeletRoot()), liveMountDevices(stacks), true
}

// parseConsumerBinds picks our FUSE consumer binds out of a mount table. Only
// <root>/pods/<uid>/volumes/kubernetes.io~csi/<pv>/mount qualifies: a subPath
// mount below it, or another driver's non-FUSE volume, is not ours to repair.
func parseConsumerBinds(stacks map[string][]rclone.HostMount, kubeletRoot string) []consumerBind {
	podsPrefix := kubeletRoot + "/pods/"
	const csiSegment = "/volumes/kubernetes.io~csi/"

	var out []consumerBind
	for path, mounts := range stacks {
		top := mounts[len(mounts)-1]
		if !strings.HasPrefix(top.FSType, "fuse") {
			continue
		}
		rest, found := strings.CutPrefix(path, podsPrefix)
		if !found {
			continue
		}
		podUID, rest, found := strings.Cut(rest, csiSegment)
		if !found || podUID == "" || strings.Contains(podUID, "/") {
			continue
		}
		pvName, leaf, found := strings.Cut(rest, "/")
		if !found || leaf != "mount" || pvName == "" {
			continue
		}
		out = append(out, consumerBind{
			path: path, podUID: podUID, pvName: pvName, depth: len(mounts), dev: top.Dev,
		})
	}
	return out
}

// liveMountDevices is the set of devices the host actually resolves: the
// topmost mount at each path. A device outside it is either buried under a
// newer mount or gone from the table altogether — either way nothing reaching
// the host through a path can still get to it.
func liveMountDevices(stacks map[string][]rclone.HostMount) map[string]bool {
	live := make(map[string]bool, len(stacks))
	for _, mounts := range stacks {
		live[mounts[len(mounts)-1].Dev] = true
	}
	return live
}

// containerStrandedDevices returns the FUSE devices a pod's containers hold
// that the host no longer resolves. A container's mounts are its own entries
// in its own namespace, so re-binding on the host never moves it: it keeps
// reading the old superblock, whose VFS is shut down, and everything that
// reaches the backend comes back EIO while stat() reports the path healthy.
func containerStrandedDevices(podUID string, live map[string]bool) []string {
	var stranded []string
	seen := make(map[string]bool)
	for _, pid := range podContainerPIDs(podUID) {
		data, err := readFileBounded(fmt.Sprintf("/proc/%d/mountinfo", pid), 5*time.Second)
		if err != nil {
			continue
		}
		for _, mounts := range rclone.ParseMountStacks(data) {
			for _, m := range mounts {
				if strings.HasPrefix(m.FSType, "fuse") && !live[m.Dev] && !seen[m.Dev] {
					seen[m.Dev] = true
					stranded = append(stranded, m.Dev)
				}
			}
		}
	}
	return stranded
}

// maxStrandedRestartsPerSweep staggers recovery: re-binding is harmless, but a
// node whose every consumer is stranded must not be restarted all at once.
const maxStrandedRestartsPerSweep = 1

// repairStrandedConsumers recovers consumers reading a mount the host no longer
// resolves — a bind left stacked under a newer one, or a container holding a
// superblock a re-bind moved on from. Both read EIO from a shut-down VFS while
// stat() on the bind path reports healthy, so nothing else on the node notices.
func (ns *NodeServer) repairStrandedConsumers() {
	binds, live, known := consumerBinds()
	if !known || len(binds) == 0 {
		return
	}

	type broken struct {
		bind     consumerBind
		stranded []string
	}
	var todo []broken
	for _, b := range binds {
		stranded := containerStrandedDevices(b.podUID, live)
		if b.depth > 1 || len(stranded) > 0 {
			todo = append(todo, broken{b, stranded})
		}
	}
	if len(todo) == 0 {
		return
	}
	klog.Warningf("Found %d consumer bind(s) whose mount the host no longer resolves", len(todo))

	staged := ns.stagedVolumesByPV()
	restarts := 0
	for _, t := range todo {
		sv, found := staged[t.bind.pvName]
		if !found {
			klog.Warningf("Consumer bind %s is stale but its volume is not staged here; leaving it alone", t.bind.path)
			continue
		}
		// A volume mid-setup or mid-drain owns its own mounts.
		if ns.s3SyncMgr.isVolumeSetupInProgress(sv.volumeID) || ns.s3SyncMgr.isBackgroundDraining(sv.volumeID) {
			continue
		}

		consumers := ns.podsUsingPVC(sv.pvcNamespace, sv.pvcName)
		pod := podByUID(consumers, t.bind.podUID)

		// A departed pod's bind is kubelet's to unwind, and re-binding for one
		// on its way out only fights its teardown.
		if pod == nil {
			klog.Warningf("Consumer bind %s is stale but no pod with that UID runs here; leaving it to kubelet",
				t.bind.path)
			continue
		}
		if pod.DeletionTimestamp != nil {
			continue
		}

		klog.Warningf("Volume %s: re-pointing %s (%d mount(s) stacked there; container holds stale device(s) %v)",
			sv.volumeID, t.bind.path, t.bind.depth, t.stranded)
		if !ns.rebindConsumerMount(sv.globalmountPath, t.bind.podUID, t.bind.pvName, fsGroupFromPods(consumers)) {
			continue
		}
		if len(t.stranded) == 0 {
			continue
		}

		// Propagation carries the re-bind into the container. Rather than infer
		// that from the pod spec, re-read what the container actually holds.
		rclone.InvalidateHostMounts()
		if _, live2, ok := consumerBinds(); ok && len(containerStrandedDevices(t.bind.podUID, live2)) == 0 {
			klog.Infof("Pod %s/%s picked the re-bind up via mount propagation for volume %s; left running",
				pod.Namespace, pod.Name, sv.volumeID)
			continue
		}
		if restarts >= maxStrandedRestartsPerSweep {
			klog.Warningf("Pod %s/%s still holds a stale mount for volume %s; deferring its restart to a later "+
				"sweep so the whole node is not recovered at once", pod.Namespace, pod.Name, sv.volumeID)
			continue
		}
		if !ns.consumerRestartAllowed(sv.volumeID) {
			continue
		}
		klog.Warningf("Pod %s/%s cannot self-heal for volume %s and is reading a shut-down mount; restarting",
			pod.Namespace, pod.Name, sv.volumeID)
		ns.recoverConsumerPod(pod)
		restarts++
	}
}

// stagedVolume is one of this node's staged volumes, as a consumer bind path
// names it: by PV, not by volume handle.
type stagedVolume struct {
	volumeID        string
	globalmountPath string
	pvcNamespace    string
	pvcName         string
}

// stagedVolumesByPV indexes this node's staged volumes by PV name. One PV list
// for the whole sweep: resolving each bind on its own re-listed every PV in the
// cluster per bind.
func (ns *NodeServer) stagedVolumesByPV() map[string]stagedVolume {
	csiPluginPath := resolveKubeletRoot() + "/plugins/kubernetes.io/csi/" + DriverName
	entries, err := os.ReadDir(csiPluginPath)
	if err != nil || ns.clientset == nil {
		return nil
	}
	stagingPaths := make(map[string]string, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		volumeDir := filepath.Join(csiPluginPath, e.Name())
		if id := ns.getVolumeIDFromVolData(volumeDir); id != "" {
			stagingPaths[id] = filepath.Join(volumeDir, "globalmount")
		}
	}

	pvs, err := ns.clientset.CoreV1().PersistentVolumes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		klog.Warningf("Could not list PVs to resolve stacked consumer binds: %v", err)
		return nil
	}
	out := make(map[string]stagedVolume, len(stagingPaths))
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != DriverName {
			continue
		}
		path, staged := stagingPaths[pv.Spec.CSI.VolumeHandle]
		if !staged {
			continue
		}
		sv := stagedVolume{volumeID: pv.Spec.CSI.VolumeHandle, globalmountPath: path}
		if pv.Spec.ClaimRef != nil {
			sv.pvcNamespace, sv.pvcName = pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name
		}
		out[pv.Name] = sv
	}
	return out
}

// podByUID picks a pod out of a consumer list.
func podByUID(pods []corev1.Pod, podUID string) *corev1.Pod {
	for i := range pods {
		if string(pods[i].UID) == podUID {
			return &pods[i]
		}
	}
	return nil
}

// readFileBounded reads a file without parking the caller forever: procfs
// mount tables take the kernel mount lock, which a wedged umount can hold.
func readFileBounded(path string, timeout time.Duration) (string, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := os.ReadFile(path)
		done <- result{data, err}
	}()
	select {
	case r := <-done:
		return string(r.data), r.err
	case <-time.After(timeout):
		return "", fmt.Errorf("reading %s blocked for %s", path, timeout)
	}
}

// maxMountStackUnwind bounds how many stacked mounts unmountHostStack removes;
// past it something is re-mounting behind us and looping only hides it.
const maxMountStackUnwind = 10

// unmountHostStack detaches every mount stacked at path in the HOST namespace,
// where bindMount makes them. Unmounting from our own namespace can leave the
// host entry in place, and stat() never sees what the next bind buries.
func unmountHostStack(path string) error {
	for i := range maxMountStackUnwind {
		depth, known := rclone.HostMountDepth(path)
		if !known {
			return fmt.Errorf("host mount table unreadable; cannot confirm %s is unmounted", path)
		}
		if depth == 0 {
			return nil
		}
		// Only the topmost mount can still be live, and callers close the
		// device under it as soon as this returns; anything buried is a stale
		// bind that just needs detaching.
		if err := unmountHostOnce(path, i == 0); err != nil {
			return err
		}
		rclone.InvalidateHostMounts()
	}
	depth, _ := rclone.HostMountDepth(path)
	return fmt.Errorf("%s still carries %d stacked mount(s) after %d unmounts", path, depth, maxMountStackUnwind)
}

// unmountHostOnce removes the topmost mount at path in the host namespace.
// With tryNormal it attempts a non-lazy unmount first: only that proves the
// filesystem is detached rather than merely scheduled for it.
func unmountHostOnce(path string, tryNormal bool) error {
	if tryNormal {
		err := runCmdBounded(30*time.Second, "nsenter", "-t", "1", "-m", "umount", path)
		if err == nil {
			return nil
		}
		klog.Warningf("Normal unmount of %s failed: %v, trying lazy unmount", path, err)
	}
	if err := runCmdBounded(30*time.Second, "nsenter", "-t", "1", "-m", "umount", "-l", path); err != nil {
		return fmt.Errorf("umount -l %s in the host namespace: %w", path, err)
	}
	return nil
}

// unbindTerminatingConsumer lazily unmounts a terminating pod's CSI bind so a
// dead FUSE doesn't wedge kubelet's teardown. No-op if not published here.
func (ns *NodeServer) unbindTerminatingConsumer(podUID, pvName string) {
	if pvName == "" {
		return
	}
	targetPath := filepath.Join(resolveKubeletRoot(), "pods", podUID,
		"volumes", "kubernetes.io~csi", pvName, "mount")
	if _, err := os.Stat(filepath.Dir(targetPath)); err != nil {
		return // not published on this node
	}
	if err := unmountHostStack(targetPath); err != nil {
		klog.Warningf("Pod %s: could not unbind %s: %v", podUID, targetPath, err)
	}
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

// consumerMountHealthy reports whether the re-mount reached a consumer pod's
// CSI mount path: the bind must be backed by the same superblock (st_dev) as
// the repaired globalmount — statfs alone is FUSE-local and passes on a bind
// still pointing at a detached dead mount. ENOENT means not published here.
func (ns *NodeServer) consumerMountHealthy(podUID, pvName, globalmountPath string) bool {
	if pvName == "" {
		return false
	}
	mountPath := filepath.Join(resolveKubeletRoot(), "pods", podUID,
		"volumes", "kubernetes.io~csi", pvName, "mount")

	// stat() resolves only the topmost mount, so a stale bind buried under a
	// fresh one reads as healthy while the container still holds the buried
	// one — whose VFS is shut down, so every backend read there is EIO.
	if depth, known := rclone.HostMountDepth(mountPath); known && depth > 1 {
		klog.Warningf("Pod %s: %d mounts stacked at %s; the container may still hold a buried one",
			podUID, depth, mountPath)
		return false
	}

	want, err := statBounded(globalmountPath, 5*time.Second)
	if err != nil {
		return false
	}
	for attempt := 0; attempt < 6; attempt++ {
		got, err := statBounded(mountPath, 5*time.Second)
		if errors.Is(err, syscall.ENOENT) {
			return true
		}
		if err == nil && got.Dev == want.Dev {
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
			if err := unmountHostStack(mountPath); err != nil {
				klog.Warningf("Pod %s: could not unbind %s: %v", podUID, mountPath, err)
			}

			// Delete the pod to trigger restart (if managed by a controller like Deployment)
			ns.deletePodByUID(podUID)
		}
	}
}

// staleTerminationGrace is the slack past a pod's deletion grace period before
// we treat it as wedged in termination.
const staleTerminationGrace = 30 * time.Second

// sweepStuckTerminatingConsumers force-deletes our volumes' consumers that are
// wedged in termination. Recovery deletes a consumer to re-publish it; if its
// volume teardown hangs, the pod object never goes away, and a StatefulSet
// cannot recreate that ordinal — the workload stays down indefinitely.
// Reconcile alone does not catch this, because once the mount looks healthy
// the pod is never revisited.
func (ns *NodeServer) sweepStuckTerminatingConsumers() {
	if ns.clientset == nil {
		return
	}
	ctx := context.Background()
	pods, err := ns.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + ns.driver.nodeID,
	})
	if err != nil {
		klog.V(4).Infof("stuck-terminating sweep: could not list pods: %v", err)
		return
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp == nil || !podStuckTerminating(pod) {
			continue
		}
		// Only our own consumers, and only pods a controller will recreate.
		if !ns.podUsesOurVolumes(ctx, pod) || metav1.GetControllerOf(pod) == nil {
			continue
		}
		klog.Warningf("Pod %s/%s has been terminating since %s with a volume of ours — force-deleting so its "+
			"controller can recreate it", pod.Namespace, pod.Name, pod.DeletionTimestamp.Format(time.RFC3339))
		ns.forceDeletePod(ctx, pod)
	}
}

// podUsesOurVolumes reports whether any of the pod's PVCs is backed by a PV
// belonging to this driver.
func (ns *NodeServer) podUsesOurVolumes(ctx context.Context, pod *corev1.Pod) bool {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		pvc, err := ns.clientset.CoreV1().PersistentVolumeClaims(pod.Namespace).
			Get(ctx, v.PersistentVolumeClaim.ClaimName, metav1.GetOptions{})
		if err != nil || pvc.Spec.VolumeName == "" {
			continue
		}
		pv, err := ns.clientset.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
		if err != nil || pv.Spec.CSI == nil {
			continue
		}
		if pv.Spec.CSI.Driver == DriverName {
			return true
		}
	}
	return false
}

// podStuckTerminating reports whether a pod has been terminating past its
// deletion grace period (kubelet couldn't finish teardown).
func podStuckTerminating(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp == nil {
		return false
	}
	grace := int64(0)
	if pod.DeletionGracePeriodSeconds != nil {
		grace = *pod.DeletionGracePeriodSeconds
	}
	deadline := pod.DeletionTimestamp.Add(time.Duration(grace)*time.Second + staleTerminationGrace)
	return time.Now().After(deadline)
}

// deletePodByUID deletes a pod by UID to recover from a stale S3 mount, using
// the least-invasive action: Succeeded pods are left alone, a controller-owned
// pod wedged in termination is force-deleted (grace 0), and otherwise a normal
// delete is issued (or skipped when its controller will handle recreation).
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

	for i := range pods.Items {
		pod := &pods.Items[i]
		if string(pod.UID) != podUID {
			continue
		}

		switch {
		case pod.Status.Phase == corev1.PodSucceeded:
			// Completed (e.g. a finished backup Job); deleting it confuses its controller.
			klog.Infof("Pod %s/%s (UID: %s) is Succeeded, skipping delete",
				pod.Namespace, pod.Name, podUID)
		case podStuckTerminating(pod):
			// Wedged teardown blocks StatefulSet recreation; force-delete as a last resort.
			if metav1.GetControllerOf(pod) == nil {
				klog.Infof("Pod %s/%s (UID: %s) is stuck terminating but has no controller; not force-deleting",
					pod.Namespace, pod.Name, podUID)
			} else {
				ns.forceDeletePod(ctx, pod)
			}
		case pod.DeletionTimestamp != nil:
			klog.Infof("Pod %s/%s (UID: %s) is terminating within grace, leaving teardown to kubelet",
				pod.Namespace, pod.Name, podUID)
		case pod.Status.Phase == corev1.PodFailed:
			klog.Infof("Pod %s/%s (UID: %s) is Failed; leaving recreation to its controller",
				pod.Namespace, pod.Name, podUID)
		default:
			klog.Infof("Deleting pod %s/%s (UID: %s) to recover from stale S3 mount",
				pod.Namespace, pod.Name, podUID)
			if ns.recorder != nil {
				ns.recorder.Event(pod, corev1.EventTypeWarning, "StaleS3MountRecovery",
					"Deleting pod: its S3-backed volume mount went stale and cannot self-heal via mount propagation")
			}
			if err := ns.clientset.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
				klog.Errorf("Failed to delete pod %s/%s: %v", pod.Namespace, pod.Name, err)
			} else {
				klog.Infof("Successfully deleted pod %s/%s, it will be recreated by its controller",
					pod.Namespace, pod.Name)
			}
		}
		return
	}

	klog.Warningf("Could not find pod with UID %s to delete", podUID)
}

// forceDeletePod removes a pod object immediately (grace 0) so its controller
// can recreate it — e.g. a StatefulSet blocked by a lingering ordinal.
func (ns *NodeServer) forceDeletePod(ctx context.Context, pod *corev1.Pod) {
	klog.Warningf("Force-deleting stuck-terminating pod %s/%s (UID: %s) so its controller can recreate it",
		pod.Namespace, pod.Name, pod.UID)
	if ns.recorder != nil {
		ns.recorder.Event(pod, corev1.EventTypeWarning, "StaleS3MountRecovery",
			"Force-deleting pod wedged in termination by a stale S3-backed volume mount so its controller can recreate it")
	}
	grace := int64(0)
	if err := ns.clientset.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
	}); err != nil && !k8serrors.IsNotFound(err) {
		klog.Errorf("Failed to force-delete pod %s/%s: %v", pod.Namespace, pod.Name, err)
	}
}

// A mount must be missing from the VFS registry on this many consecutive ticks
// before it counts as a zombie: one tick can race a mount being registered.
const zombieVFSConfirmations = 2

// vfsRegistryMissing reports whether a live FUSE mount has no VFS behind it in
// rclone, confirmed across consecutive ticks. Every request such a mount serves
// returns EIO, which is invisible to statfs and to cached listings.
func (ns *NodeServer) vfsRegistryMissing(volumeDir string, vfsNames map[string]int, known bool) bool {
	if !known {
		return false
	}
	volumeID := ns.getVolumeIDFromVolData(volumeDir)
	// A mount still being set up has no VFS yet, and a drain owns the volume's
	// cache — neither is a zombie.
	if volumeID == "" || ns.s3SyncMgr.isVolumeSetupInProgress(volumeID) ||
		ns.s3SyncMgr.isBackgroundDraining(volumeID) {
		return false
	}
	if vfsNames[ns.expectedFSName(volumeID)] > 0 {
		ns.zombieVFSStrikes.Delete(volumeID)
		return false
	}
	strikes := 1
	if prev, ok := ns.zombieVFSStrikes.Load(volumeID); ok {
		strikes = prev.(int) + 1
	}
	if strikes < zombieVFSConfirmations {
		ns.zombieVFSStrikes.Store(volumeID, strikes)
		klog.Warningf("Volume %s: its FUSE mount is live but rclone has no VFS for it (%d/%d confirmations)",
			volumeID, strikes, zombieVFSConfirmations)
		return false
	}
	ns.zombieVFSStrikes.Delete(volumeID)
	return true
}

// expectedFSName is the rclone remote this volume's mount should be registered
// under: the live manager's name, or the persisted one after a driver restart.
func (ns *NodeServer) expectedFSName(volumeID string) string {
	ns.s3SyncMgr.mutex.RLock()
	mm := ns.s3SyncMgr.mountManagers[volumeID]
	ns.s3SyncMgr.mutex.RUnlock()
	if mm != nil {
		return mm.FSName()
	}
	return rclone.FSNameForVolume(volumeID)
}

// A read probe parked this long is itself the symptom: the mount is wedged, and
// reporting it healthy while the probe never returns hides that forever.
const vfsProbeStuckAfter = 2 * time.Minute

// mountVFSResponsive reports whether a FUSE mount can serve real reads,
// catching a cancelled-VFS zombie that passes statfs but fails real ops. Times
// out as healthy (slow backend), and runs one probe per path at a time.
func (ns *NodeServer) mountVFSResponsive(globalmountPath string) bool {
	if prev, inFlight := ns.vfsProbesInFlight.LoadOrStore(globalmountPath, time.Now()); inFlight {
		if since := time.Since(prev.(time.Time)); since > vfsProbeStuckAfter {
			klog.Warningf("S3 mount %s: read probe has been blocked for %s (wedged FUSE); treating as unresponsive",
				globalmountPath, since.Round(time.Second))
			return false
		}
		return true
	}

	done := make(chan error, 1)
	go func() {
		defer ns.vfsProbesInFlight.Delete(globalmountPath)
		done <- probeMountReads(globalmountPath)
	}()

	select {
	case err := <-done:
		return err == nil
	case <-time.After(5 * time.Second):
		return true
	}
}

// abortFUSEConnection aborts the kernel FUSE connections backing mountPoint via
// /sys/fs/fuse/connections/<dev-minor>/abort. Pending and future requests fail
// with ECONNABORTED, releasing D-state waiters a wedged serve loop stranded.
// The device ids come from mountinfo, which never stats the mount.
//
// The host namespace is the authority, as it is for every other mount check:
// our own /proc/self/mountinfo does not carry mounts rclone made and propagated
// out of a replaced sandbox, so reading it aborted nothing and every wedged
// mount stayed wedged. Aborts every FUSE layer stacked at the path — a buried
// one still serves whoever opened it before the mount above it appeared.
func abortFUSEConnection(mountPoint string) {
	// A cached table can name a device minor the kernel has since recycled for
	// another FUSE mount, and aborting that one would break an innocent volume.
	rclone.InvalidateHostMounts()
	stacks, ok := rclone.HostMountStacksOK()
	if !ok {
		// Unreadable is usually a wedged umount holding the kernel mount lock —
		// exactly when we need the abort — so fall back to our own view rather
		// than skip. Worse than the host's, better than nothing.
		data, err := os.ReadFile("/proc/self/mountinfo")
		if err != nil {
			klog.Warningf("abortFUSEConnection %s: cannot read any mount table: %v", mountPoint, err)
			return
		}
		stacks = rclone.ParseMountStacks(string(data))
	}

	aborted := 0
	for _, m := range stacks[mountPoint] {
		if !strings.HasPrefix(m.FSType, "fuse") {
			continue
		}
		_, minor, found := strings.Cut(m.Dev, ":")
		if !found {
			continue
		}
		if err := writeFUSEAbort(minor); err != nil {
			klog.Warningf("abortFUSEConnection %s: aborting connection %s failed: %v", mountPoint, minor, err)
			continue
		}
		aborted++
		klog.Infof("abortFUSEConnection %s: aborted FUSE connection %s", mountPoint, minor)
	}
	if aborted == 0 {
		klog.V(4).Infof("abortFUSEConnection %s: no FUSE mount there to abort", mountPoint)
	}
}

// fusectl is a separate filesystem mounted only on the host, so this path
// reads as empty in our container and every abort through it silently does
// nothing. Fall back to the host mount namespace.
const fuseConnectionsDir = "/sys/fs/fuse/connections"

// writeFUSEAbort aborts a FUSE connection by device minor, ours then the host's.
func writeFUSEAbort(minor string) error {
	abortPath := fuseConnectionsDir + "/" + minor + "/abort"
	if err := os.WriteFile(abortPath, []byte("1"), 0200); err == nil {
		return nil
	}
	return runCmdBounded(15*time.Second, "nsenter", "-t", "1", "-m", "sh", "-c",
		"echo 1 > "+abortPath)
}

// listFUSEConnections returns the kernel's FUSE connection minors.
func listFUSEConnections() []string {
	if entries, err := os.ReadDir(fuseConnectionsDir); err == nil && len(entries) > 0 {
		conns := make([]string, 0, len(entries))
		for _, e := range entries {
			conns = append(conns, e.Name())
		}
		return conns
	}
	out, err := runCmdBoundedOutput(15*time.Second, "nsenter", "-t", "1", "-m", "ls", fuseConnectionsDir)
	if err != nil {
		klog.V(4).Infof("Could not list FUSE connections in the host namespace: %v", err)
		return nil
	}
	return strings.Fields(out)
}

// runCmdBounded runs a command with a hard timeout, returning without waiting
// on a child wedged in uninterruptible sleep (stalled disk, dead mount) — the
// caller's goroutine must never hang on node-level I/O stalls.
func runCmdBounded(timeout time.Duration, name string, arg ...string) error {
	_, err := runCmdBoundedOutput(timeout, name, arg...)
	return err
}

// runCmdBoundedOutput is runCmdBounded returning the command's stdout.
func runCmdBoundedOutput(timeout time.Duration, name string, arg ...string) (string, error) {
	cmd := exec.Command(name, arg...)
	var out strings.Builder
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.String(), err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return out.String(), fmt.Errorf("%s %v timed out after %s", name, arg, timeout)
	}
}

// ioStallThresholdPct is the PSI io "full avg10" percentage above which the
// node is considered mid-stall.
const ioStallThresholdPct = 40.0

// nodeIOStalled reports whether the node is in an I/O stall wave: mounts
// probed during a stall look dead, and destructive recovery then only adds
// churn to a node that needs quiet.
func nodeIOStalled() (float64, bool) {
	data, err := os.ReadFile("/proc/pressure/io")
	if err != nil {
		return 0, false
	}
	return parseIOPressure(string(data))
}

// parseIOPressure extracts the "full avg10" percentage from PSI io content.
func parseIOPressure(s string) (float64, bool) {
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "full ") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(f, "avg10="); ok {
				if pct, err := strconv.ParseFloat(v, 64); err == nil {
					return pct, pct >= ioStallThresholdPct
				}
			}
		}
	}
	return 0, false
}

// abortOrphanedFUSEConnections aborts FUSE connections whose superblock is not
// mounted in any mount namespace on the host — detached remnants of dead
// mounts whose queued requests hold processes (typically a terminating
// predecessor driver pod, which then wedges holding our ports) in
// uninterruptible sleep. Runs once at startup; needs hostPID and writable /sys.
func abortOrphanedFUSEConnections() {
	conns := listFUSEConnections()
	if len(conns) == 0 {
		return
	}

	// Collect anon-device minors mounted in ANY mount namespace (deduped via
	// ns links), so another container's private FUSE is never called orphaned.
	mounted := map[string]bool{}
	procs, _ := os.ReadDir("/proc")
	seenNS := map[string]bool{}
	for _, p := range procs {
		if _, err := strconv.Atoi(p.Name()); err != nil {
			continue
		}
		nsLink, err := os.Readlink("/proc/" + p.Name() + "/ns/mnt")
		if err != nil || seenNS[nsLink] {
			continue
		}
		seenNS[nsLink] = true
		data, err := os.ReadFile("/proc/" + p.Name() + "/mountinfo")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			if maj, minor, ok := strings.Cut(fields[2], ":"); ok && maj == "0" {
				mounted[minor] = true
			}
		}
	}

	for _, minor := range conns {
		if mounted[minor] {
			continue
		}
		if err := writeFUSEAbort(minor); err != nil {
			klog.Warningf("Could not abort orphaned FUSE connection %s: %v", minor, err)
			continue
		}
		klog.Warningf("Aborted orphaned FUSE connection %s (detached superblock, mounted in no namespace) — "+
			"releases processes wedged on dead mounts", minor)
	}
}

// statfsProbe is a single-flight statfs whose result later callers can await.
type statfsProbe struct {
	done chan struct{}
	err  error
}

// statfsBounded runs statfs with a timeout. A wedged FUSE blocks statfs in
// uninterruptible sleep; one probe goroutine per path is kept, and callers
// finding it in flight await the same result — no per-tick goroutine leak.
func (ns *NodeServer) statfsBounded(path string, timeout time.Duration) (error, bool) {
	v, loaded := ns.statfsProbesInFlight.LoadOrStore(path, &statfsProbe{done: make(chan struct{})})
	p := v.(*statfsProbe)
	if !loaded {
		go func() {
			var st syscall.Statfs_t
			p.err = syscall.Statfs(path, &st)
			ns.statfsProbesInFlight.Delete(path)
			close(p.done)
		}()
	}
	select {
	case <-p.done:
		return p.err, false
	case <-time.After(timeout):
		return nil, true
	}
}

// statBounded stats a path without parking the caller on a wedged FUSE.
func statBounded(path string, timeout time.Duration) (syscall.Stat_t, error) {
	type result struct {
		st  syscall.Stat_t
		err error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		r.err = syscall.Stat(path, &r.st)
		done <- r
	}()
	select {
	case r := <-done:
		return r.st, r.err
	case <-time.After(timeout):
		return syscall.Stat_t{}, fmt.Errorf("stat %s blocked for %s (wedged FUSE)", path, timeout)
	}
}

// removeAllBounded removes a tree without blocking on a wedged mount under it.
func removeAllBounded(path string, timeout time.Duration) {
	done := make(chan error, 1)
	go func() { done <- os.RemoveAll(path) }()
	select {
	case err := <-done:
		if err != nil {
			klog.Warningf("Failed to remove %s: %v", path, err)
		}
	case <-time.After(timeout):
		klog.Warningf("Removing %s blocked for %s (a wedged mount is still attached there); giving up", path, timeout)
	}
}

// mkdirAllBounded creates a tree without blocking on a wedged mount there.
func mkdirAllBounded(path string, perm os.FileMode, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- os.MkdirAll(path, perm) }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("mkdir %s blocked for %s (a wedged mount is still attached there)", path, timeout)
	}
}

// isMountDeadErr matches errnos that indicate a dead or zombie mount rather
// than a normal filesystem condition.
func isMountDeadErr(err error) bool {
	return errors.Is(err, syscall.EIO) || errors.Is(err, syscall.ENOTCONN) || errors.Is(err, syscall.ESTALE)
}

// probeMountReads exercises a mount past the dir cache: lists breadth-first
// (depth ≤3) and opens the first regular file — a cached listing can succeed
// while a zombie VFS fails every open. Raw syscalls only; see rclone.ReadDirRaw.
func probeMountReads(root string) error {
	dirs := []string{root}
	for depth := 0; depth < 3 && len(dirs) > 0; depth++ {
		var next []string
		for _, dir := range dirs {
			entries, err := rclone.ReadDirRaw(dir, 512)
			if err != nil && isMountDeadErr(err) {
				return err
			}
			for _, e := range entries {
				p := filepath.Join(dir, e.Name)
				if e.IsDir {
					if len(next) < 8 {
						next = append(next, p)
					}
					continue
				}
				// DT_UNKNOWN is worth probing: the open decides what it is.
				if !e.IsRegular && !e.Unknown {
					continue
				}
				if err := rclone.OpenProbeRaw(p); err != nil {
					if isMountDeadErr(err) {
						return err
					}
					continue
				}
				return nil
			}
		}
		dirs = next
	}
	// No regular file reachable (e.g. empty volume): listings sufficed.
	return nil
}

// isFUSEMountPoint reports whether path is served by FUSE in the HOST mount
// namespace. Our own /proc/mounts can hold entries the host has dropped, and
// trusting those makes the driver blind to a volume that is dead for every
// consumer.
func (ns *NodeServer) isFUSEMountPoint(path string) bool {
	return rclone.IsHostFUSEMount(path)
}

// verifyS3StagingLive returns nil only when stagingPath is a host-visible
// rclone FUSE mount that still answers. The gate before binding into a pod:
// the directory under a missing mount is writable node-local storage.
func (ns *NodeServer) verifyS3StagingLive(stagingPath string) error {
	isFUSE, known := rclone.HostFUSEMountState(stagingPath)
	if !known {
		return fmt.Errorf("the host mount table is unreadable, so %s cannot be confirmed as a live FUSE mount", stagingPath)
	}
	if !isFUSE {
		return fmt.Errorf("%s is not an rclone FUSE mount in the host namespace; binding it would expose the "+
			"empty directory underneath and write plaintext to the node disk instead of S3", stagingPath)
	}

	// statfs is FUSE-local and surfaces ENOTCONN/ESTALE/EIO on a dead mount.
	statErr, timedOut := ns.statfsBounded(stagingPath, 5*time.Second)
	if timedOut {
		return fmt.Errorf("statfs on %s blocked for 5s: the FUSE daemon is wedged", stagingPath)
	}
	if statErr != nil {
		return fmt.Errorf("statfs on %s failed: %w", stagingPath, statErr)
	}
	return nil
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
	if err := runCmdBounded(30*time.Second, "umount", "-l", mountPath); err != nil {
		klog.Warningf("umount -l failed for %s: %v", mountPath, err)
	}

	removeAllBounded(mountPath, 60*time.Second)
}

// cleanupOrphanedVFSCacheDirs removes VFS cache directories for volumes whose
// PVs have been deleted. This handles the case where a PV is deleted while the
// CSI driver pod is down, leaving orphaned cache directories.
func (ns *NodeServer) cleanupOrphanedVFSCacheDirs() {
	if ns.clientset == nil {
		klog.Warning("Kubernetes client not available, skipping orphaned VFS cache cleanup")
		return
	}

	ctx := context.Background()
	memo := make(map[string]bool)
	isActive := func(volumeID string) bool {
		if known, ok := memo[volumeID]; ok {
			return known
		}
		_, err := ns.clientset.CoreV1().PersistentVolumes().Get(ctx, volumeID, metav1.GetOptions{})
		active := true
		if k8serrors.IsNotFound(err) {
			active = false
		} else if err != nil {
			// API error — treat as active to be safe
			klog.Warningf("Error checking PV %s: %v, treating as active", volumeID, err)
		}
		memo[volumeID] = active
		return active
	}

	rclone.CleanupOrphanedVFSCacheDirs(isActive)
	klog.Infof("Orphaned VFS cache cleanup completed")
}
