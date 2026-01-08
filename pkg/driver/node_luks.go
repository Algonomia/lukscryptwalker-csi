package driver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
)

// =============================================================================
// LUKS Device Setup and Management
// =============================================================================

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

// cleanupVolumeStaging cleans up volume staging resources
func (ns *NodeServer) cleanupVolumeStaging(volumeID, stagingTargetPath string) error {
	// Unmount the staging target (only if mounted)
	if ns.isMountPoint(stagingTargetPath) {
		// Sync filesystem to ensure all dirty pages are flushed before unmounting
		// This is important because we close the LUKS device immediately after
		klog.Infof("Syncing filesystem before unmount for volume %s", volumeID)
		syncCmd := exec.Command("sync", "-f", stagingTargetPath)
		if err := syncCmd.Run(); err != nil {
			klog.Warningf("sync -f failed for %s: %v, trying global sync", stagingTargetPath, err)
			// Fallback to global sync
			globalSyncCmd := exec.Command("sync")
			_ = globalSyncCmd.Run()
		}

		if err := ns.unmountPath(stagingTargetPath); err != nil {
			klog.Errorf("Failed to unmount staging path %s: %v", stagingTargetPath, err)
			// Continue with cleanup even if unmount fails
		}
	} else {
		klog.V(4).Infof("Staging path %s is not mounted, skipping unmount", stagingTargetPath)
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

	// Clean up staging target directory (kubelet expects this to be removed)
	// Use RemoveAll for S3 volumes which may have files inside
	if err := os.RemoveAll(stagingTargetPath); err != nil && !os.IsNotExist(err) {
		klog.V(4).Infof("Could not remove staging directory %s: %v (may be handled by kubelet)", stagingTargetPath, err)
	} else if err == nil {
		klog.Infof("Removed staging directory: %s", stagingTargetPath)
	}

	return nil
}

// restoreVolumeStaging restores a volume's staging mount after node reboot
func (ns *NodeServer) restoreVolumeStaging(ctx context.Context, volumeID, stagingTargetPath string, volumeContext, secrets map[string]string) error {
	klog.Infof("Restoring volume staging for volume %s at %s", volumeID, stagingTargetPath)

	// Check if this is an S3-only volume
	if ns.isS3Backend(volumeContext) {
		klog.Infof("Restoring S3-only volume %s", volumeID)

		// Create staging directory if it doesn't exist
		if err := os.MkdirAll(stagingTargetPath, 0777); err != nil {
			return fmt.Errorf("failed to create staging directory: %v", err)
		}

		// Setup S3 sync
		if err := ns.setupS3Sync(volumeID, stagingTargetPath, volumeContext, secrets); err != nil {
			return fmt.Errorf("failed to setup S3 sync during restore: %v", err)
		}

		klog.Infof("Successfully restored S3-only volume %s", volumeID)
		return nil
	}

	// For LUKS volumes, validate backing file exists
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
		_ = ns.luksManager.CloseLUKS(mapperName) // Best effort cleanup on failure
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
	klog.Infof("Retrieving StorageClass parameters for volumeID: %s", volumeID)
	scParams, err := ns.GetStorageClassParametersByVolumeID(ctx, volumeID)
	if err != nil {
		klog.Errorf("Failed to get StorageClass parameters for volumeID %s: %v", volumeID, err)
		return nil, status.Errorf(codes.FailedPrecondition, "StorageClass parameters for volumeID %s not found: %v", volumeID, err)
	}
	klog.Infof("Successfully retrieved StorageClass parameters for volumeID %s", volumeID)

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
// Loop Device Operations
// =============================================================================

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

	// Check if already mounted at target (idempotency)
	if ns.isMountPoint(targetPath) {
		if ns.isMountedFrom(targetPath, devicePath) {
			klog.Infof("Device %s already mounted at %s", devicePath, targetPath)
			return nil
		}
		// Mounted from different device - unmount first
		klog.Warningf("Target %s is mounted from different device, unmounting", targetPath)
		if err := ns.unmountPath(targetPath); err != nil {
			return fmt.Errorf("failed to unmount stale mount at %s: %v", targetPath, err)
		}
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
// Backing File Operations
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
