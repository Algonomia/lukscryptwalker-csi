package driver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/lukscryptwalker-csi/pkg/luks"
	"github.com/lukscryptwalker-csi/pkg/secrets"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog"
)

// Constants
const (
	DefaultLocalPath = "/opt/local-path-provisioner"
)

// NodeServer implements the CSI Node service
type NodeServer struct {
	csi.UnimplementedNodeServer
	driver         *Driver
	luksManager    *luks.LUKSManager
	clientset      kubernetes.Interface
	secretsManager *secrets.SecretsManager
	s3SyncMgr      *S3SyncManager
}

// NewNodeServer creates a new NodeServer instance
func NewNodeServer(d *Driver) *NodeServer {
	clientset := initializeKubernetesClient()

	ns := &NodeServer{
		driver:         d,
		luksManager:    luks.NewLUKSManager(),
		clientset:      clientset,
		secretsManager: secrets.NewSecretsManager(clientset),
		s3SyncMgr:      NewS3SyncManager(),
	}

	// Clean up and restore stale S3 mounts from previous crashes/restarts
	ns.cleanupStaleS3Mounts()

	// Clean up orphaned volume directories (from deleted PVCs)
	ns.cleanupOrphanedVolumes()

	// Start background goroutine to periodically check for stale S3 mounts
	go ns.runStaleS3MountChecker()

	return ns
}

// runStaleS3MountChecker periodically checks for and handles stale S3 mounts
func (ns *NodeServer) runStaleS3MountChecker() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		ns.cleanupStaleS3Mounts()
	}
}

// cleanupOrphanedVolumes removes volume directories for PVs that no longer exist.
// This is needed because we preserve backing files across pod restarts, but they
// should be cleaned up when the PV is actually deleted.
func (ns *NodeServer) cleanupOrphanedVolumes() {
	if ns.clientset == nil {
		klog.Warning("Kubernetes client not available, skipping orphaned volume cleanup")
		return
	}

	localPath := os.Getenv("CSI_LOCAL_PATH")
	if localPath == "" {
		localPath = DefaultLocalPath
	}

	klog.Infof("Checking for orphaned volume directories in %s", localPath)

	entries, err := os.ReadDir(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			klog.V(4).Infof("Local path %s does not exist, no cleanup needed", localPath)
			return
		}
		klog.Warningf("Failed to read local path directory %s: %v", localPath, err)
		return
	}

	ctx := context.Background()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		volumeID := entry.Name()
		// Skip directories that don't look like PVC IDs
		if !strings.HasPrefix(volumeID, "pvc-") {
			continue
		}

		volumeDir := filepath.Join(localPath, volumeID)

		// Check if this directory belongs to our driver by looking for our backing file
		backingFile := GenerateBackingFilePath(volumeDir, volumeID)
		if _, err := os.Stat(backingFile); os.IsNotExist(err) {
			// No backing file with our naming convention - not our volume
			klog.V(4).Infof("Directory %s has no LUKS backing file, skipping (belongs to another driver)", volumeDir)
			continue
		}

		// Check if PV exists for this volume ID
		pv, err := ns.clientset.CoreV1().PersistentVolumes().Get(ctx, volumeID, metav1.GetOptions{})
		if err == nil {
			// PV exists - verify it's our driver before keeping
			if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == DriverName {
				klog.V(4).Infof("PV %s exists and belongs to our driver, keeping volume directory", volumeID)
			}
			continue
		}

		if !k8serrors.IsNotFound(err) {
			// API error, skip this volume to be safe
			klog.Warningf("Error checking PV %s: %v, skipping cleanup", volumeID, err)
			continue
		}

		// PV not found and backing file exists - this is an orphaned volume from our driver
		klog.Infof("Found orphaned volume directory (PV deleted): %s", volumeDir)

		// Clean up S3 sync if present (safe to call even if not S3 volume)
		if err := ns.cleanupS3Sync(volumeID); err != nil {
			klog.Warningf("Failed to cleanup S3 sync for orphaned volume %s: %v", volumeID, err)
		}

		// Clean up LUKS device (pass empty staging path for orphan cleanup)
		if err := ns.cleanupVolumeStaging(volumeID, ""); err != nil {
			klog.Warningf("Failed to cleanup LUKS for orphaned volume %s: %v, skipping removal", volumeID, err)
			continue
		}

		// Remove the orphaned directory
		if err := os.RemoveAll(volumeDir); err != nil {
			klog.Warningf("Failed to remove orphaned volume directory %s: %v", volumeDir, err)
		} else {
			klog.Infof("Successfully removed orphaned volume directory: %s", volumeDir)
		}
	}

	klog.Infof("Orphaned volume cleanup completed")
}

// initializeKubernetesClient sets up the Kubernetes client with proper error handling
func initializeKubernetesClient() kubernetes.Interface {
	config, err := rest.InClusterConfig()
	if err != nil {
		klog.Errorf("Failed to create in-cluster config: %v", err)
		return nil
	}

	// Configure client timeouts for better network handling
	config.Timeout = 10 * 1000000000 // 10 seconds in nanoseconds
	config.QPS = 20
	config.Burst = 30

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Errorf("Failed to create kubernetes clientset: %v", err)
		return nil
	}

	klog.Infof("Successfully initialized Kubernetes client with API server: %s", config.Host)
	return clientset
}

// =============================================================================
// CSI Node Service Implementation
// =============================================================================

// NodeStageVolume stages a volume on the node
func (ns *NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	klog.Infof("NodeStageVolume called for volume %s", req.GetVolumeId())

	// Validate request parameters
	if err := ns.validateStageVolumeRequest(req); err != nil {
		return nil, err
	}

	volumeID := req.GetVolumeId()
	stagingTargetPath := req.GetStagingTargetPath()

	// Check if volume is already staged (idempotency)
	if ns.isVolumeStaged(volumeID, stagingTargetPath) {
		klog.Infof("Volume %s is already staged at %s, returning success", volumeID, stagingTargetPath)
		return &csi.NodeStageVolumeResponse{}, nil
	}

	// Prepare volume staging
	stageParams, err := ns.prepareVolumeStaging(req)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to prepare volume staging: %v", err)
	}

	// Choose storage backend
	if ns.isS3Backend(req.GetVolumeContext()) {
		// S3 backend - no LUKS, files encrypted individually
		if err := ns.setupS3Volume(stageParams, req.GetVolumeContext(), req.GetSecrets()); err != nil {
			return nil, status.Errorf(codes.Internal, "Failed to setup S3 volume: %v", err)
		}
	} else {
		// Local LUKS backend - traditional approach
		if err := ns.setupLUKSDevice(stageParams); err != nil {
			return nil, status.Errorf(codes.Internal, "Failed to setup LUKS device: %v", err)
		}

		// Mount and configure the LUKS volume
		if err := ns.mountAndConfigureVolume(stageParams); err != nil {
			return nil, status.Errorf(codes.Internal, "Failed to mount and configure volume: %v", err)
		}
	}

	klog.Infof("Successfully staged volume %s", volumeID)
	return &csi.NodeStageVolumeResponse{}, nil
}

// NodeUnstageVolume unstages a volume from the node
func (ns *NodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	klog.Infof("NodeUnstageVolume called with request: %+v", req)

	// Validate request parameters
	if err := ns.validateUnstageVolumeRequest(req); err != nil {
		return nil, err
	}

	volumeID := req.GetVolumeId()
	stagingTargetPath := req.GetStagingTargetPath()

	// Cleanup S3 sync first if configured
	if err := ns.cleanupS3Sync(volumeID); err != nil {
		klog.Errorf("Failed to cleanup S3 sync for volume %s: %v", volumeID, err)
		// Don't fail the unstage operation
	}

	// Perform cleanup operations
	if err := ns.cleanupVolumeStaging(volumeID, stagingTargetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to cleanup volume staging: %v", err)
	}

	klog.Infof("Successfully unstaged volume %s", volumeID)
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodePublishVolume publishes a volume to make it available to workloads
func (ns *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	klog.Infof("NodePublishVolume called for volume %s", req.GetVolumeId())

	// Validate request parameters
	if err := ns.validatePublishVolumeRequest(req); err != nil {
		return nil, err
	}

	volumeID := req.GetVolumeId()
	stagingTargetPath := req.GetStagingTargetPath()
	targetPath := req.GetTargetPath()
	fsGroup := ns.extractFsGroup(req.GetVolumeContext())

	// Ensure volume is staged, restore if needed after reboot
	if err := ns.ensureVolumeStaged(ctx, req); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to ensure volume staged: %v", err)
	}

	// Create bind mount
	if err := ns.bindMount(stagingTargetPath, targetPath, req.GetReadonly(), fsGroup); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to bind mount: %v", err)
	}

	klog.Infof("Successfully published volume %s", volumeID)
	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume unpublishes a volume
func (ns *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	klog.Infof("NodeUnpublishVolume called with request: %+v", req)

	if req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	// Unmount target path
	if err := ns.unmountPath(req.GetTargetPath()); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to unmount target path: %v", err)
	}

	klog.Infof("Successfully unpublished volume at %s", req.GetTargetPath())
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeExpandVolume expands a volume
func (ns *NodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	klog.Infof("NodeExpandVolume called with request: %+v", req)

	// Validate request parameters
	expandParams, err := ns.validateAndPrepareExpansionRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	// Check if volume is already expanded (idempotency)
	if ns.isVolumeAlreadyExpanded(expandParams.backingFile, expandParams.requestedBytes) {
		klog.Infof("Volume %s is already expanded to %d bytes, returning success",
			expandParams.volumeID, expandParams.requestedBytes)
		return &csi.NodeExpandVolumeResponse{CapacityBytes: expandParams.requestedBytes}, nil
	}

	// Perform volume expansion
	if err := ns.performVolumeExpansion(ctx, expandParams); err != nil {
		return nil, err
	}

	klog.Infof("Successfully expanded volume %s to %d bytes", expandParams.volumeID, expandParams.requestedBytes)
	return &csi.NodeExpandVolumeResponse{CapacityBytes: expandParams.requestedBytes}, nil
}

// NodeGetCapabilities returns the capabilities of the node service
func (ns *NodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
					},
				},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
					},
				},
			},
		},
	}, nil
}

// NodeGetInfo returns information about the node
func (ns *NodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: ns.driver.nodeID,
	}, nil
}

// NodeGetVolumeStats returns volume statistics
func (ns *NodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeGetVolumeStats is not implemented")
}

// =============================================================================
// Request Validation Methods
// =============================================================================

// validateStageVolumeRequest validates the NodeStageVolume request
func (ns *NodeServer) validateStageVolumeRequest(req *csi.NodeStageVolumeRequest) error {
	if req.GetVolumeId() == "" {
		return status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if req.GetStagingTargetPath() == "" {
		return status.Error(codes.InvalidArgument, "Staging target path missing in request")
	}
	if req.GetVolumeCapability() == nil {
		return status.Error(codes.InvalidArgument, "Volume capability missing in request")
	}
	return nil
}

// validateUnstageVolumeRequest validates the NodeUnstageVolume request
func (ns *NodeServer) validateUnstageVolumeRequest(req *csi.NodeUnstageVolumeRequest) error {
	if req.GetVolumeId() == "" {
		return status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if req.GetStagingTargetPath() == "" {
		return status.Error(codes.InvalidArgument, "Staging target path missing in request")
	}
	return nil
}

// validatePublishVolumeRequest validates the NodePublishVolume request
func (ns *NodeServer) validatePublishVolumeRequest(req *csi.NodePublishVolumeRequest) error {
	if req.GetVolumeId() == "" {
		return status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if req.GetStagingTargetPath() == "" {
		return status.Error(codes.InvalidArgument, "Staging target path missing in request")
	}
	if req.GetTargetPath() == "" {
		return status.Error(codes.InvalidArgument, "Target path missing in request")
	}
	return nil
}

// =============================================================================
// Volume Staging Preparation
// =============================================================================

// StagingParameters holds parameters for volume staging operations
type StagingParameters struct {
	volumeID          string
	stagingTargetPath string
	localPath         string
	backingFile       string
	passphrase        string
	mapperName        string
	mappedDevice      string
	fsGroup           *int64
	volumeCapability  *csi.VolumeCapability
}

// prepareVolumeStaging prepares parameters for volume staging
// For S3 volumes, backing file and LUKS-related fields are not populated
func (ns *NodeServer) prepareVolumeStaging(req *csi.NodeStageVolumeRequest) (*StagingParameters, error) {
	volumeID := req.GetVolumeId()
	fsGroup := ns.extractFsGroup(req.GetVolumeContext())

	params := &StagingParameters{
		volumeID:          volumeID,
		stagingTargetPath: req.GetStagingTargetPath(),
		fsGroup:           fsGroup,
		volumeCapability:  req.GetVolumeCapability(),
	}

	// S3 volumes don't use local LUKS backing files
	if ns.isS3Backend(req.GetVolumeContext()) {
		return params, nil
	}

	// LUKS volumes: setup local path and backing file
	localPath := GetLocalPath(volumeID)
	backingFile := GenerateBackingFilePath(localPath, volumeID)

	// Ensure local path directory exists
	if err := os.MkdirAll(localPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create local path directory %s: %v", localPath, err)
	}

	// Create backing file if it doesn't exist
	if err := ns.ensureBackingFileExists(req, backingFile); err != nil {
		return nil, fmt.Errorf("failed to ensure backing file: %v", err)
	}

	// Get passphrase
	passphrase, err := ns.getPassphraseFromRequest(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get passphrase: %v", err)
	}

	mapperName := ns.luksManager.GenerateMapperName(volumeID)
	mappedDevice := ns.luksManager.GetMappedDevicePath(mapperName)

	params.localPath = localPath
	params.backingFile = backingFile
	params.passphrase = passphrase
	params.mapperName = mapperName
	params.mappedDevice = mappedDevice

	return params, nil
}

// ensureVolumeStaged ensures the volume is staged, restoring if needed after reboot
func (ns *NodeServer) ensureVolumeStaged(ctx context.Context, req *csi.NodePublishVolumeRequest) error {
	volumeID := req.GetVolumeId()
	stagingTargetPath := req.GetStagingTargetPath()
	volumeContext := req.GetVolumeContext()

	// Check if volume is already staged
	if ns.isS3Backend(volumeContext) {
		// S3 volumes: check if mount point exists
		if ns.isMountPoint(stagingTargetPath) {
			return nil
		}
		klog.Infof("S3 volume %s not staged at %s, attempting to restore", volumeID, stagingTargetPath)
		return ns.restoreS3VolumeStaging(volumeID, stagingTargetPath, volumeContext, req.GetSecrets())
	}

	// LUKS volumes: use existing check
	if !ns.isVolumeStaged(volumeID, stagingTargetPath) {
		klog.Infof("LUKS volume %s not staged at %s, attempting to restore", volumeID, stagingTargetPath)
		return ns.restoreLUKSVolumeStaging(ctx, volumeID, stagingTargetPath, volumeContext)
	}

	return nil
}

// =============================================================================
// System Operations
// =============================================================================

// isMountPoint checks if the given path is a mount point
func (ns *NodeServer) isMountPoint(path string) bool {
	cmd := exec.Command("mountpoint", "-q", path)
	return cmd.Run() == nil
}

// isMountedFrom checks if the given path is mounted from the specified device
func (ns *NodeServer) isMountedFrom(path, device string) bool {
	cmd := exec.Command("findmnt", "-n", "-o", "SOURCE", path)
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	mountedFrom := strings.TrimSpace(string(output))
	return mountedFrom == device
}

// unmountPath unmounts a filesystem path
func (ns *NodeServer) unmountPath(targetPath string) error {
	cmd := exec.Command("umount", targetPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to unmount %s: %v", targetPath, err)
	}
	return nil
}

// bindMount creates a simple bind mount from source to target
func (ns *NodeServer) bindMount(sourcePath, targetPath string, readonly bool, fsGroup *int64) error {
	// Create target directory in host namespace using nsenter
	mkdirArgs := []string{"-t", "1", "-m", "-u", "mkdir", "-p", targetPath}
	klog.Infof("Creating target directory with nsenter: nsenter %v", mkdirArgs)
	mkdirCmd := exec.Command("nsenter", mkdirArgs...)
	if err := mkdirCmd.Run(); err != nil {
		return fmt.Errorf("failed to create target directory in host namespace: %v", err)
	}

	// Create bind mount using nsenter to operate in host namespace
	args := []string{"-t", "1", "-m", "-u", "mount", "--bind", sourcePath, targetPath}

	klog.Infof("Executing nsenter bind mount: nsenter %v", args)
	cmd := exec.Command("nsenter", args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to nsenter bind mount %s to %s: %v", sourcePath, targetPath, err)
	}

	// Set readonly if requested
	if readonly {
		cmd = exec.Command("mount", "-o", "remount,ro", targetPath)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to remount as readonly: %v", err)
		}
	}

	return nil
}

// =============================================================================
// Permission and Security Management
// =============================================================================

// extractFsGroup extracts the fsGroup from volume context
func (ns *NodeServer) extractFsGroup(volumeContext map[string]string) *int64 {
	// Check for manual fsGroup override
	if fsGroupStr, exists := volumeContext["fsGroup"]; exists {
		if fsGroup, err := strconv.ParseInt(fsGroupStr, 10, 64); err == nil {
			klog.Infof("Using manual fsGroup %d from StorageClass parameters", fsGroup)
			return &fsGroup
		}
	}

	// Fall back to auto-detection from pod security context
	if fsGroupStr, exists := volumeContext["csi.storage.k8s.io/pod.spec.securityContext.fsGroup"]; exists {
		if fsGroup, err := strconv.ParseInt(fsGroupStr, 10, 64); err == nil {
			klog.Infof("Auto-detected fsGroup %d from pod volume context", fsGroup)
			return &fsGroup
		}
	}

	klog.Infof("No fsGroup found - using default permissions")
	return nil
}

// applyFsGroupPermissions applies fsGroup ownership to the bind mount target
func (ns *NodeServer) applyFsGroupPermissions(targetPath string, fsGroup int64) error {
	klog.Infof("Applying fsGroup %d permissions recursively to %s", fsGroup, targetPath)

	// Recursive chown using nsenter to operate in host namespace
	chownCmd := exec.Command("nsenter", "-t", "1", "-m", "-u", "chown", "-R", fmt.Sprintf("%d:%d", fsGroup, fsGroup), targetPath)
	if output, err := chownCmd.CombinedOutput(); err != nil {
		klog.Errorf("nsenter chown command failed: %v, output: %s", err, string(output))
		return fmt.Errorf("failed to recursively apply fsGroup with nsenter: %v, output: %s", err, string(output))
	} else {
		klog.Infof("nsenter chown command successful, output: %s", string(output))
	}

	// Ensure group writable using nsenter
	chmodCmd := exec.Command("nsenter", "-t", "1", "-m", "-u", "chmod", "-R", "775", targetPath)
	if output, err := chmodCmd.CombinedOutput(); err != nil {
		klog.Errorf("nsenter chmod command failed: %v, output: %s", err, string(output))
		return fmt.Errorf("failed to recursively chmod with nsenter: %v, output: %s", err, string(output))
	} else {
		klog.Infof("nsenter chmod command successful, output: %s", string(output))
	}

	klog.Infof("Successfully applied fsGroup %d permissions recursively to %s", fsGroup, targetPath)
	return nil
}

