package driver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/lukscryptwalker-csi/pkg/luks"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Constants
const (
	DefaultLocalPath     = "/opt/local-path-provisioner"
	SecretNamespaceKey   = "csi.storage.k8s.io/node-stage-secret-namespace"
	SecretNameKey        = "csi.storage.k8s.io/node-stage-secret-name"
	PassphraseKeyParam   = "passphraseKey"
	DefaultPassphraseKey = "passphrase"
)

// NodeServer implements the CSI Node service
type NodeServer struct {
	driver      *Driver
	luksManager *luks.LUKSManager
	clientset   kubernetes.Interface
}

// NewNodeServer creates a new NodeServer instance
func NewNodeServer(d *Driver) *NodeServer {
	clientset := initializeKubernetesClient()
	
	return &NodeServer{
		driver:      d,
		luksManager: luks.NewLUKSManager(),
		clientset:   clientset,
	}
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

	// Setup LUKS encryption
	if err := ns.setupLUKSDevice(stageParams); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to setup LUKS device: %v", err)
	}

	// Mount and configure the volume
	if err := ns.mountAndConfigureVolume(stageParams); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to mount and configure volume: %v", err)
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
// Volume Staging Operations
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
func (ns *NodeServer) prepareVolumeStaging(req *csi.NodeStageVolumeRequest) (*StagingParameters, error) {
	volumeID := req.GetVolumeId()
	localPath := GetLocalPath(volumeID)
	backingFile := GenerateBackingFilePath(localPath, volumeID)
	fsGroup := ns.extractFsGroup(req.GetVolumeContext())

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

	return &StagingParameters{
		volumeID:          volumeID,
		stagingTargetPath: req.GetStagingTargetPath(),
		localPath:         localPath,
		backingFile:       backingFile,
		passphrase:        passphrase,
		mapperName:        mapperName,
		mappedDevice:      mappedDevice,
		fsGroup:           fsGroup,
		volumeCapability:  req.GetVolumeCapability(),
	}, nil
}

// setupLUKSDevice sets up and opens the LUKS encrypted device
func (ns *NodeServer) setupLUKSDevice(params *StagingParameters) error {
	return ns.luksManager.FormatAndOpenLUKS(params.backingFile, params.mapperName, params.passphrase)
}

// mountAndConfigureVolume mounts the device and applies fsGroup permissions
func (ns *NodeServer) mountAndConfigureVolume(params *StagingParameters) error {
	// Format the device if needed
	if err := ns.formatDevice(params.mappedDevice, params.volumeCapability); err != nil {
		return fmt.Errorf("failed to format device: %v", err)
	}

	// Mount the device
	if err := ns.mountDevice(params.mappedDevice, params.stagingTargetPath, params.volumeCapability); err != nil {
		return fmt.Errorf("failed to mount device: %v", err)
	}

	// Apply fsGroup permissions to the real mounted filesystem before bind mounting
	if params.fsGroup != nil {
		if err := ns.applyFsGroupPermissions(params.stagingTargetPath, *params.fsGroup); err != nil {
			return fmt.Errorf("failed to apply fsGroup permissions to staging path: %v", err)
		}
	}

	return nil
}

// ensureVolumeStaged ensures the volume is staged, restoring if needed after reboot
func (ns *NodeServer) ensureVolumeStaged(ctx context.Context, req *csi.NodePublishVolumeRequest) error {
	volumeID := req.GetVolumeId()
	stagingTargetPath := req.GetStagingTargetPath()

	if !ns.isVolumeStaged(volumeID, stagingTargetPath) {
		klog.Infof("Volume %s not staged at %s, attempting to restore after reboot", volumeID, stagingTargetPath)
		return ns.restoreVolumeStaging(ctx, volumeID, stagingTargetPath, req.GetVolumeContext(), req.GetSecrets())
	}

	return nil
}

// cleanupVolumeStaging cleans up volume staging resources
func (ns *NodeServer) cleanupVolumeStaging(volumeID, stagingTargetPath string) error {
	// Unmount the staging target
	if err := ns.unmountPath(stagingTargetPath); err != nil {
		klog.Errorf("Failed to unmount staging path %s: %v", stagingTargetPath, err)
		// Continue with cleanup even if unmount fails
	}

	// Close LUKS device
	mapperName := ns.luksManager.GenerateMapperName(volumeID)
	if err := ns.luksManager.CloseLUKS(mapperName); err != nil {
		return fmt.Errorf("failed to close LUKS device: %v", err)
	}

	// Clean up backing files and directories
	localPath := GetLocalPath(volumeID)
	if err := os.RemoveAll(localPath); err != nil && !os.IsNotExist(err) {
		klog.Warningf("Failed to clean up volume directory %s: %v", localPath, err)
		// Don't fail the unstage operation due to cleanup issues
	} else if !os.IsNotExist(err) {
		klog.Infof("Successfully cleaned up volume directory: %s", localPath)
	}

	return nil
}

// restoreVolumeStaging restores a volume's staging mount after node reboot
func (ns *NodeServer) restoreVolumeStaging(ctx context.Context, volumeID, stagingTargetPath string, volumeContext, secrets map[string]string) error {
	klog.Infof("Restoring volume staging for volume %s at %s", volumeID, stagingTargetPath)

	// Validate backing file exists
	backingFile := GenerateBackingFilePath(GetLocalPath(volumeID), volumeID)
	if _, err := os.Stat(backingFile); os.IsNotExist(err) {
		return fmt.Errorf("backing file %s does not exist", backingFile)
	}

	// Get passphrase
	passphrase, err := ns.getPassphraseForRestore(ctx, volumeID, volumeContext, secrets)
	if err != nil {
		return fmt.Errorf("failed to get passphrase: %v", err)
	}

	// Restore LUKS device and mount
	if err := ns.restoreLUKSDeviceAndMount(volumeID, backingFile, stagingTargetPath, passphrase, volumeContext); err != nil {
		return fmt.Errorf("failed to restore LUKS device and mount: %v", err)
	}

	klog.Infof("Successfully restored volume staging for volume %s", volumeID)
	return nil
}

// =============================================================================
// Volume Expansion Operations
// =============================================================================

// ExpansionParameters holds parameters for volume expansion operations
type ExpansionParameters struct {
	volumeID       string
	volumePath     string
	requestedBytes int64
	backingFile    string
	mapperName     string
	mappedDevice   string
	scParams       map[string]string
}

// validateAndPrepareExpansionRequest validates and prepares expansion parameters
func (ns *NodeServer) validateAndPrepareExpansionRequest(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*ExpansionParameters, error) {
	klog.Infof("Starting validation and preparation for volume expansion request")
	
	// Validate basic parameters
	if req.GetVolumeId() == "" {
		klog.Errorf("Volume ID missing in expansion request")
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if req.GetVolumePath() == "" {
		klog.Errorf("Volume path missing in expansion request")
		return nil, status.Error(codes.InvalidArgument, "Volume path missing in request")
	}
	if req.GetCapacityRange() == nil {
		klog.Errorf("Capacity range missing in expansion request")
		return nil, status.Error(codes.InvalidArgument, "Capacity range missing in request")
	}

	volumeID := req.GetVolumeId()
	requestedBytes := req.GetCapacityRange().GetRequiredBytes()
	if requestedBytes == 0 {
		requestedBytes = req.GetCapacityRange().GetLimitBytes()
	}
	
	klog.Infof("Validated basic parameters - volumeID: %s, requestedBytes: %d", volumeID, requestedBytes)

	// Get StorageClass parameters for passphrase retrieval
	klog.Infof("Retrieving PV for volumeID: %s", volumeID)
	pv, err := ns.getPVByVolumeID(ctx, volumeID)
	if err != nil {
		klog.Errorf("Failed to get PV for volumeID %s: %v", volumeID, err)
		return nil, status.Errorf(codes.FailedPrecondition, "PV for volumeID %s not found: %v", volumeID, err)
	}
	klog.Infof("Successfully retrieved PV for volumeID %s, StorageClass: %s", volumeID, pv.Spec.StorageClassName)

	klog.Infof("Retrieving StorageClass parameters for: %s", pv.Spec.StorageClassName)
	scParams, err := ns.getStorageClassParameters(ctx, pv.Spec.StorageClassName)
	if err != nil {
		klog.Errorf("Failed to get StorageClass %s parameters: %v", pv.Spec.StorageClassName, err)
		return nil, status.Errorf(codes.FailedPrecondition, "Parameters for StorageClass %s not found: %v", 
			pv.Spec.StorageClassName, err)
	}
	klog.Infof("Successfully retrieved StorageClass parameters")

	// Prepare paths and device names
	localPath := GetLocalPath(volumeID)
	backingFile := GenerateBackingFilePath(localPath, volumeID)
	mapperName := ns.luksManager.GenerateMapperName(volumeID)
	mappedDevice := ns.luksManager.GetMappedDevicePath(mapperName)
	
	klog.Infof("Generated paths - localPath: %s, backingFile: %s, mapperName: %s, mappedDevice: %s", 
		localPath, backingFile, mapperName, mappedDevice)

	// Validate LUKS device is opened
	klog.Infof("Checking if LUKS device %s is opened", mapperName)
	if !ns.luksManager.IsLUKSOpened(mapperName) {
		klog.Errorf("LUKS device %s is not opened - expansion cannot proceed", mapperName)
		return nil, status.Errorf(codes.FailedPrecondition, "LUKS device %s is not opened", mapperName)
	}
	klog.Infof("LUKS device %s is opened and ready for expansion", mapperName)

	klog.Infof("Successfully completed validation and preparation for volume expansion")
	return &ExpansionParameters{
		volumeID:       volumeID,
		volumePath:     req.GetVolumePath(),
		requestedBytes: requestedBytes,
		backingFile:    backingFile,
		mapperName:     mapperName,
		mappedDevice:   mappedDevice,
		scParams:       scParams,
	}, nil
}

// performVolumeExpansion performs the actual volume expansion operations
func (ns *NodeServer) performVolumeExpansion(ctx context.Context, params *ExpansionParameters) error {
	klog.Infof("Starting volume expansion for %s to %d bytes", params.volumeID, params.requestedBytes)
	
	// Expand backing file
	klog.Infof("Expanding backing file %s to %d bytes", params.backingFile, params.requestedBytes)
	if err := ExpandBackingFile(params.backingFile, params.requestedBytes); err != nil {
		klog.Errorf("Failed to expand backing file %s: %v", params.backingFile, err)
		return status.Errorf(codes.Internal, "Failed to expand backing file: %v", err)
	}
	klog.Infof("Successfully expanded backing file %s", params.backingFile)

	// Refresh loop device
	klog.Infof("Refreshing loop device for backing file %s", params.backingFile)
	if err := ns.refreshLoopDevice(params.backingFile); err != nil {
		klog.Warningf("Failed to refresh loop device for %s: %v", params.backingFile, err)
	} else {
		klog.Infof("Successfully refreshed loop device for backing file %s", params.backingFile)
	}

	// Get passphrase and resize LUKS
	klog.Infof("Retrieving passphrase for LUKS resize of device %s", params.mapperName)
	passphrase, err := ns.getPassphraseForExpansion(ctx, params.scParams)
	if err != nil {
		klog.Errorf("Failed to get passphrase for LUKS resize: %v", err)
		return status.Errorf(codes.Internal, "Failed to get passphrase for LUKS resize: %v", err)
	}
	klog.Infof("Successfully retrieved passphrase for LUKS resize")

	klog.Infof("Resizing LUKS device %s", params.mapperName)
	if err := ns.luksManager.ResizeLUKS(params.mapperName, passphrase); err != nil {
		klog.Errorf("Failed to resize LUKS device %s: %v", params.mapperName, err)
		return status.Errorf(codes.Internal, "Failed to resize LUKS device: %v", err)
	}
	klog.Infof("Successfully resized LUKS device %s", params.mapperName)

	// Resize filesystem
	klog.Infof("Resizing filesystem on device %s (volume path: %s)", params.mappedDevice, params.volumePath)
	if err := ns.resizeFilesystem(params.mappedDevice, params.volumePath); err != nil {
		klog.Errorf("Failed to resize filesystem on %s: %v", params.mappedDevice, err)
		return status.Errorf(codes.Internal, "Failed to resize filesystem: %v", err)
	}
	klog.Infof("Successfully resized filesystem on device %s", params.mappedDevice)

	klog.Infof("Volume expansion completed successfully for %s", params.volumeID)
	return nil
}

// =============================================================================
// Volume State Management
// =============================================================================

// isVolumeStaged checks if a volume is already staged at the given path
func (ns *NodeServer) isVolumeStaged(volumeID, stagingTargetPath string) bool {
	mapperName := ns.luksManager.GenerateMapperName(volumeID)
	
	// Check if LUKS device is opened
	if !ns.luksManager.IsLUKSOpened(mapperName) {
		klog.Infof("LUKS device %s is not opened", mapperName)
		return false
	}
	
	// Check if staging target path is mounted
	if !ns.isMountPoint(stagingTargetPath) {
		klog.Infof("Staging target path %s is not mounted", stagingTargetPath)
		return false
	}
	
	// Verify mount is from our mapped device
	mappedDevice := ns.luksManager.GetMappedDevicePath(mapperName)
	if !ns.isMountedFrom(stagingTargetPath, mappedDevice) {
		klog.Infof("Staging target path %s is not mounted from our device %s", stagingTargetPath, mappedDevice)
		return false
	}
	
	klog.Infof("Volume %s is already staged at %s", volumeID, stagingTargetPath)
	return true
}

// isVolumeAlreadyExpanded checks if the backing file is already expanded to the requested size
func (ns *NodeServer) isVolumeAlreadyExpanded(backingFile string, requestedBytes int64) bool {
	fileInfo, err := os.Stat(backingFile)
	if err != nil {
		klog.Infof("Cannot stat backing file %s: %v", backingFile, err)
		return false
	}
	
	currentSize := fileInfo.Size()
	if currentSize >= requestedBytes {
		klog.Infof("Backing file %s is already %d bytes (requested: %d bytes)", backingFile, currentSize, requestedBytes)
		return true
	}
	
	klog.Infof("Backing file %s is %d bytes, needs expansion to %d bytes", backingFile, currentSize, requestedBytes)
	return false
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

// refreshLoopDevice finds and refreshes the loop device associated with a backing file
func (ns *NodeServer) refreshLoopDevice(backingFile string) error {
	klog.Infof("Refreshing loop device for backing file: %s", backingFile)
	
	// Find associated loop device
	cmd := exec.Command("losetup", "-j", backingFile)
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to find loop device for %s: %v", backingFile, err)
	}
	
	outputStr := strings.TrimSpace(string(output))
	if outputStr == "" {
		return fmt.Errorf("no loop device found for backing file %s", backingFile)
	}
	
	// Parse and refresh loop device
	lines := strings.Split(outputStr, "\n")
	for _, line := range lines {
		if strings.Contains(line, ":") {
			parts := strings.Split(line, ":")
			if len(parts) > 0 {
				loopDevice := strings.TrimSpace(parts[0])
				klog.Infof("Found loop device %s for backing file %s", loopDevice, backingFile)
				
				refreshCmd := exec.Command("losetup", "-c", loopDevice)
				if err := refreshCmd.Run(); err != nil {
					return fmt.Errorf("failed to refresh loop device %s: %v", loopDevice, err)
				}
				
				klog.Infof("Successfully refreshed loop device %s", loopDevice)
				return nil
			}
		}
	}
	
	return fmt.Errorf("could not parse loop device from output: %s", outputStr)
}

// =============================================================================
// Filesystem Operations
// =============================================================================

// formatDevice formats a device with the specified filesystem
func (ns *NodeServer) formatDevice(devicePath string, capability *csi.VolumeCapability) error {
	mount := capability.GetMount()
	if mount == nil {
		return fmt.Errorf("only mount access type is supported")
	}

	fsType := mount.GetFsType()
	if fsType == "" {
		fsType = "ext4"
	}

	// Check if device is already formatted
	cmd := exec.Command("blkid", devicePath)
	if cmd.Run() == nil {
		klog.Infof("Device %s is already formatted", devicePath)
		return nil
	}

	klog.Infof("Formatting device %s with filesystem %s", devicePath, fsType)
	
	var formatCmd *exec.Cmd
	switch fsType {
	case "ext4":
		formatCmd = exec.Command("mkfs.ext4", "-F", devicePath)
	case "ext3":
		formatCmd = exec.Command("mkfs.ext3", "-F", devicePath)
	case "xfs":
		formatCmd = exec.Command("mkfs.xfs", "-f", devicePath)
	default:
		return fmt.Errorf("unsupported filesystem type: %s", fsType)
	}

	if err := formatCmd.Run(); err != nil {
		return fmt.Errorf("failed to format device %s: %v", devicePath, err)
	}

	return nil
}

// mountDevice mounts a device to the specified target path
func (ns *NodeServer) mountDevice(devicePath, targetPath string, capability *csi.VolumeCapability) error {
	mount := capability.GetMount()
	if mount == nil {
		return fmt.Errorf("only mount access type is supported")
	}

	// Create target directory
	if err := os.MkdirAll(targetPath, 0777); err != nil {
		return fmt.Errorf("failed to create target directory: %v", err)
	}

	fsType := mount.GetFsType()
	if fsType == "" {
		fsType = "ext4"
	}

	// Prepare mount arguments
	args := []string{"-t", fsType}
	mountOptions := mount.GetMountFlags()
	if len(mountOptions) > 0 {
		args = append(args, "-o", strings.Join(mountOptions, ","))
	}
	args = append(args, devicePath, targetPath)

	cmd := exec.Command("mount", args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to mount device %s to %s: %v", devicePath, targetPath, err)
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

// resizeFilesystem resizes the filesystem on the given device
func (ns *NodeServer) resizeFilesystem(devicePath, volumePath string) error {
	klog.Infof("Resizing filesystem on device %s", devicePath)

	// Detect filesystem type
	cmd := exec.Command("blkid", "-s", "TYPE", "-o", "value", devicePath)
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to detect filesystem type on %s: %v", devicePath, err)
	}

	fsType := strings.TrimSpace(string(output))
	if fsType == "" {
		return fmt.Errorf("no filesystem found on device %s", devicePath)
	}

	klog.Infof("Detected filesystem type: %s on device %s", fsType, devicePath)

	// Choose appropriate resize command
	var resizeCmd *exec.Cmd
	switch fsType {
	case "ext2", "ext3", "ext4":
		resizeCmd = exec.Command("resize2fs", devicePath)
	case "xfs":
		resizeCmd = exec.Command("xfs_growfs", volumePath)
	default:
		return fmt.Errorf("unsupported filesystem type for resize: %s", fsType)
	}

	if err := resizeCmd.Run(); err != nil {
		return fmt.Errorf("failed to resize %s filesystem on %s: %v", fsType, devicePath, err)
	}

	klog.Infof("Successfully resized %s filesystem on device %s", fsType, devicePath)
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


// =============================================================================
// Passphrase and Secret Management
// =============================================================================

// getPassphraseFromRequest extracts passphrase from the staging request
func (ns *NodeServer) getPassphraseFromRequest(req *csi.NodeStageVolumeRequest) (string, error) {
	passphraseKey := req.GetVolumeContext()[PassphraseKeyParam]
	if passphraseKey == "" {
		passphraseKey = DefaultPassphraseKey
	}
	
	return ns.getPassphrase(req.GetSecrets(), passphraseKey)
}

// getPassphraseForRestore retrieves passphrase for volume restore operations
func (ns *NodeServer) getPassphraseForRestore(ctx context.Context, volumeID string, volumeContext, secrets map[string]string) (string, error) {
	passphraseKey := volumeContext[PassphraseKeyParam]
	if passphraseKey == "" {
		passphraseKey = DefaultPassphraseKey
	}

	// Try from provided secrets first
	if len(secrets) > 0 {
		if passphrase, err := ns.getPassphrase(secrets, passphraseKey); err == nil {
			return passphrase, nil
		}
		klog.Warningf("Failed to get passphrase from provided secrets")
	}
	
	// Fallback to StorageClass parameters
	pv, err := ns.getPVByVolumeID(ctx, volumeID)
	if err != nil {
		return "", fmt.Errorf("failed to get PV for volume %s: %v", volumeID, err)
	}
	
	scParams, err := ns.getStorageClassParameters(ctx, pv.Spec.StorageClassName)
	if err != nil {
		return "", fmt.Errorf("failed to get StorageClass parameters: %v", err)
	}
	
	return ns.getPassphraseForExpansion(ctx, scParams)
}

// getPassphrase retrieves passphrase from secrets map
func (ns *NodeServer) getPassphrase(secrets map[string]string, passphraseKey string) (string, error) {
	if passphrase, ok := secrets[passphraseKey]; ok {
		return passphrase, nil
	}
	return "", fmt.Errorf("passphrase not found in secrets with key '%s'", passphraseKey)
}

// getPassphraseForExpansion retrieves passphrase for volume expansion
func (ns *NodeServer) getPassphraseForExpansion(ctx context.Context, scParams map[string]string) (string, error) {
	if ns.clientset == nil {
		return "", fmt.Errorf("kubernetes clientset not available")
	}

	secretName, ok := scParams["csi.storage.k8s.io/node-stage-secret-name"]
	if !ok {
		return "", fmt.Errorf("node-stage-secret-name not found in StorageClass parameters")
	}
	
	secretNamespace, ok := scParams["csi.storage.k8s.io/node-stage-secret-namespace"]
	if !ok {
		return "", fmt.Errorf("node-stage-secret-namespace not found in StorageClass parameters")
	}
	
	passphraseKey, ok := scParams["passphraseKey"]
	if !ok {
		passphraseKey = DefaultPassphraseKey
	}

	// Retrieve the secret
	secretObj, err := ns.clientset.CoreV1().Secrets(secretNamespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get secret %s/%s: %v", secretNamespace, secretName, err)
	}

	// Convert secret data to string map
	secrets := make(map[string]string, len(secretObj.Data))
	for k, v := range secretObj.Data {
		secrets[k] = string(v)
	}

	return ns.getPassphrase(secrets, passphraseKey)
}

// =============================================================================
// Kubernetes Integration
// =============================================================================

// getPVByVolumeID finds the PV that matches the CSI volume handle
func (ns *NodeServer) getPVByVolumeID(ctx context.Context, volumeID string) (*corev1.PersistentVolume, error) {
	if ns.clientset == nil {
		return nil, fmt.Errorf("kubernetes clientset not available")
	}
	
	pvList, err := ns.clientset.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list PVs: %v", err)
	}

	for _, pv := range pvList.Items {
		if pv.Spec.CSI != nil && pv.Spec.CSI.VolumeHandle == volumeID {
			return &pv, nil
		}
	}

	return nil, fmt.Errorf("PV with volumeHandle %s not found", volumeID)
}

// getStorageClassParameters retrieves parameters from a StorageClass by name
func (ns *NodeServer) getStorageClassParameters(ctx context.Context, name string) (map[string]string, error) {
	if ns.clientset == nil {
		return nil, fmt.Errorf("kubernetes clientset not available")
	}
	
	if name == "" {
		return nil, fmt.Errorf("storageClassName is empty")
	}

	sc, err := ns.clientset.StorageV1().StorageClasses().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get StorageClass %s: %v", name, err)
	}

	return sc.Parameters, nil
}

// =============================================================================
// Utility Functions
// =============================================================================

// ensureBackingFileExists creates the backing file if it doesn't exist
func (ns *NodeServer) ensureBackingFileExists(req *csi.NodeStageVolumeRequest, backingFile string) error {
	if _, err := os.Stat(backingFile); os.IsNotExist(err) {
		capacityStr := req.GetVolumeContext()["capacity"]
		if capacityStr == "" {
			capacityStr = "1073741824" // Default 1GB in bytes
		}
		
		if err := CreateBackingFile(backingFile, capacityStr); err != nil {
			return fmt.Errorf("failed to create backing file: %v", err)
		}
	}
	
	return nil
}

// restoreLUKSDeviceAndMount restores LUKS device and creates mount
func (ns *NodeServer) restoreLUKSDeviceAndMount(volumeID, backingFile, stagingTargetPath, passphrase string, volumeContext map[string]string) error {
	mapperName := ns.luksManager.GenerateMapperName(volumeID)

	// Open LUKS device
	if err := ns.luksManager.OpenLUKS(backingFile, mapperName, passphrase); err != nil {
		return fmt.Errorf("failed to open LUKS device: %v", err)
	}

	// Create basic volume capability (assume ext4)
	volumeCapability := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{
				FsType: "ext4",
			},
		},
	}

	// Mount device
	mappedDevice := ns.luksManager.GetMappedDevicePath(mapperName)
	if err := ns.mountDevice(mappedDevice, stagingTargetPath, volumeCapability); err != nil {
		ns.luksManager.CloseLUKS(mapperName) // Cleanup on failure
		return fmt.Errorf("failed to mount device: %v", err)
	}

	// Apply fsGroup permissions if specified in volume context
	fsGroup := ns.extractFsGroup(volumeContext)
	if fsGroup != nil {
		if err := ns.applyFsGroupPermissions(stagingTargetPath, *fsGroup); err != nil {
			return fmt.Errorf("failed to apply fsGroup permissions during restore: %v", err)
		}
	}

	return nil
}
