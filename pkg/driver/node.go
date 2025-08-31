package driver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/lukscryptwalker-csi/pkg/luks"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

const (
	LocalPathKey         = "local-path"
	DefaultLocalPath     = "/opt/local-path-provisioner"
	SecretNamespaceKey   = "csi.storage.k8s.io/node-stage-secret-namespace"
	SecretNameKey        = "csi.storage.k8s.io/node-stage-secret-name"
	PassphraseKeyParam   = "passphraseKey"
	DefaultPassphraseKey = "passphrase"
)

type NodeServer struct {
	driver      *Driver
	luksManager *luks.LUKSManager
}

func NewNodeServer(d *Driver) *NodeServer {
	return &NodeServer{
		driver:      d,
		luksManager: luks.NewLUKSManager(),
	}
}

func (ns *NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	klog.Infof("NodeStageVolume called for volume %s", req.GetVolumeId())

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	stagingTargetPath := req.GetStagingTargetPath()
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Staging target path missing in request")
	}

	volumeCapability := req.GetVolumeCapability()
	if volumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capability missing in request")
	}

	// Get the local path from volume context
	localPath := req.GetVolumeContext()[LocalPathKey]
	if localPath == "" {
		localPath = filepath.Join(DefaultLocalPath, volumeID)
	}

	// Extract fsGroup from pod volume context
	fsGroup := ns.extractFsGroup(req.GetVolumeContext())

	// Ensure the local path directory exists with appropriate permissions
	if err := ns.setDirectoryPermissions(localPath, fsGroup); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to create local path directory %s: %v", localPath, err)
	}

	// Create a backing file for LUKS if it doesn't exist
	backingFile := filepath.Join(localPath, "luks.img")
	if _, err := os.Stat(backingFile); os.IsNotExist(err) {
		// Create a sparse file (1GB by default, can be made configurable)
		if err := ns.createBackingFile(backingFile, "1G"); err != nil {
			return nil, status.Errorf(codes.Internal, "Failed to create backing file: %v", err)
		}
	}

	// Get the passphrase key name from volume context
	passphraseKey := req.GetVolumeContext()[PassphraseKeyParam]
	if passphraseKey == "" {
		passphraseKey = DefaultPassphraseKey
	}

	// Get passphrase from secrets
	passphrase, err := ns.getPassphrase(req.GetSecrets(), passphraseKey)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to get passphrase: %v", err)
	}

	// Generate mapper name
	mapperName := ns.luksManager.GenerateMapperName(volumeID)

	// Format and open LUKS device
	if err := ns.luksManager.FormatAndOpenLUKS(backingFile, mapperName, passphrase); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to setup LUKS device: %v", err)
	}

	// Get the mapped device path
	mappedDevice := ns.luksManager.GetMappedDevicePath(mapperName)

	// Format the LUKS device with filesystem if needed
	if err := ns.formatDevice(mappedDevice, volumeCapability); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to format device: %v", err)
	}

	// Mount the device to staging path
	if err := ns.mountDevice(mappedDevice, stagingTargetPath, volumeCapability); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to mount device: %v", err)
	}

	// Set permissions on the mounted filesystem root directory
	if err := ns.setDirectoryPermissions(stagingTargetPath, fsGroup); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to set permissions on mounted filesystem: %v", err)
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

func (ns *NodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	klog.V(4).Infof("NodeUnstageVolume called with request: %+v", req)

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	stagingTargetPath := req.GetStagingTargetPath()
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Staging target path missing in request")
	}

	// Unmount the staging target
	if err := ns.unmountPath(stagingTargetPath); err != nil {
		klog.Errorf("Failed to unmount staging path %s: %v", stagingTargetPath, err)
		// Continue with cleanup even if unmount fails
	}

	// Close LUKS device
	mapperName := ns.luksManager.GenerateMapperName(volumeID)
	if err := ns.luksManager.CloseLUKS(mapperName); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to close LUKS device: %v", err)
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *NodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	klog.Infof("NodePublishVolume called for volume %s", req.GetVolumeId())

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	stagingTargetPath := req.GetStagingTargetPath()
	if len(stagingTargetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Staging target path missing in request")
	}

	targetPath := req.GetTargetPath()
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	// Extract fsGroup from pod volume context
	fsGroup := ns.extractFsGroup(req.GetVolumeContext())

	// Bind mount from staging to target with appropriate permissions
	if err := ns.bindMount(stagingTargetPath, targetPath, req.GetReadonly(), fsGroup); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to bind mount: %v", err)
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	klog.V(4).Infof("NodeUnpublishVolume called with request: %+v", req)

	targetPath := req.GetTargetPath()
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Target path missing in request")
	}

	// Unmount target path
	if err := ns.unmountPath(targetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to unmount target path: %v", err)
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

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

func (ns *NodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: ns.driver.nodeID,
	}, nil
}

func (ns *NodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeGetVolumeStats is not implemented")
}

func (ns *NodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	klog.V(4).Infof("NodeExpandVolume called with request: %+v", req)

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	volumePath := req.GetVolumePath()
	if len(volumePath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume path missing in request")
	}

	capacityRange := req.GetCapacityRange()
	if capacityRange == nil {
		return nil, status.Error(codes.InvalidArgument, "Capacity range missing in request")
	}

	requestedBytes := capacityRange.GetRequiredBytes()
	if requestedBytes == 0 {
		requestedBytes = capacityRange.GetLimitBytes()
	}

	klog.V(4).Infof("Expanding volume %s at path %s to %d bytes", volumeID, volumePath, requestedBytes)

	// Generate mapper name for this volume
	mapperName := ns.luksManager.GenerateMapperName(volumeID)

	// Check if LUKS device is opened
	if !ns.luksManager.IsLUKSOpened(mapperName) {
		return nil, status.Errorf(codes.FailedPrecondition, "LUKS device %s is not opened", mapperName)
	}

	// Resize the LUKS device to fill the expanded backing file
	if err := ns.luksManager.ResizeLUKS(mapperName); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to resize LUKS device: %v", err)
	}

	// Get the mapped device path
	mappedDevice := ns.luksManager.GetMappedDevicePath(mapperName)

	// Resize the filesystem
	if err := ns.resizeFilesystem(mappedDevice, volumePath); err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to resize filesystem: %v", err)
	}

	klog.V(4).Infof("Successfully expanded volume %s to %d bytes", volumeID, requestedBytes)

	return &csi.NodeExpandVolumeResponse{
		CapacityBytes: requestedBytes,
	}, nil
}

// Helper methods

func (ns *NodeServer) createBackingFile(filePath, size string) error {
	cmd := exec.Command("fallocate", "-l", size, filePath)
	if err := cmd.Run(); err != nil {
		// Fallback to dd if fallocate is not available
		cmd = exec.Command("dd", "if=/dev/zero", "of="+filePath, "bs=1G", "count=1", "seek=0")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to create backing file with dd: %v", err)
		}
	}
	return nil
}

func (ns *NodeServer) getPassphrase(secrets map[string]string, passphraseKey string) (string, error) {
	if passphrase, ok := secrets[passphraseKey]; ok {
		return passphrase, nil
	}
	return "", fmt.Errorf("passphrase not found in secrets with key '%s'", passphraseKey)
}

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
		klog.V(4).Infof("Device %s is already formatted", devicePath)
		return nil
	}

	klog.V(4).Infof("Formatting device %s with filesystem %s", devicePath, fsType)
	
	var cmd2 *exec.Cmd
	switch fsType {
	case "ext4":
		cmd2 = exec.Command("mkfs.ext4", "-F", devicePath)
	case "ext3":
		cmd2 = exec.Command("mkfs.ext3", "-F", devicePath)
	case "xfs":
		cmd2 = exec.Command("mkfs.xfs", "-f", devicePath)
	default:
		return fmt.Errorf("unsupported filesystem type: %s", fsType)
	}

	if err := cmd2.Run(); err != nil {
		return fmt.Errorf("failed to format device %s: %v", devicePath, err)
	}

	return nil
}

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

	mountOptions := []string{}
	mountOptions = append(mountOptions, mount.GetMountFlags()...)

	args := []string{"-t", fsType}
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

// extractFsGroup extracts the fsGroup from manual override or pod volume context
func (ns *NodeServer) extractFsGroup(volumeContext map[string]string) *int64 {
	// First check for manual fsGroup override in StorageClass parameters
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

// setDirectoryPermissions sets directory permissions based on fsGroup
func (ns *NodeServer) setDirectoryPermissions(path string, fsGroup *int64) error {
	// Create directory with appropriate permissions
	var mode os.FileMode = 0755 // Default permissions
	if fsGroup != nil {
		mode = 0775 // Group writable when fsGroup is set
		klog.Infof("Creating directory %s with mode 0775 and group ownership %d", path, *fsGroup)
	} else {
		klog.Infof("Creating directory %s with mode 0755 (no fsGroup)", path)
	}
	
	if err := os.MkdirAll(path, mode); err != nil {
		return fmt.Errorf("failed to create directory: %v", err)
	}
	
	// If fsGroup is specified, set both permissions and group ownership explicitly
	if fsGroup != nil {
		// Explicitly set the permissions to ensure they're correct
		if err := os.Chmod(path, mode); err != nil {
			klog.Warningf("Failed to set permissions %o: %v", mode, err)
		} else {
			klog.Infof("Successfully set permissions %o for %s", mode, path)
		}
		
		// Change both user and group ownership to fsGroup
		if err := os.Chown(path, int(*fsGroup), int(*fsGroup)); err != nil {
			klog.Warningf("Failed to change ownership to %d:%d: %v", *fsGroup, *fsGroup, err)
			// Don't fail the mount, just log the warning
		} else {
			klog.Infof("Successfully set ownership to %d:%d for %s", *fsGroup, *fsGroup, path)
		}
	}
	
	return nil
}

func (ns *NodeServer) bindMount(sourcePath, targetPath string, readonly bool, fsGroup *int64) error {
	// Create target directory with appropriate permissions
	if err := ns.setDirectoryPermissions(targetPath, fsGroup); err != nil {
		return err
	}

	args := []string{"--bind", sourcePath, targetPath}
	cmd := exec.Command("mount", args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to bind mount %s to %s: %v", sourcePath, targetPath, err)
	}

	if readonly {
		cmd = exec.Command("mount", "-o", "remount,ro", targetPath)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to remount as readonly: %v", err)
		}
	}

	return nil
}

func (ns *NodeServer) unmountPath(targetPath string) error {
	cmd := exec.Command("umount", targetPath)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to unmount %s: %v", targetPath, err)
	}
	return nil
}

func (ns *NodeServer) resizeFilesystem(devicePath, volumePath string) error {
	klog.V(4).Infof("Resizing filesystem on device %s", devicePath)

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

	klog.V(4).Infof("Detected filesystem type: %s on device %s", fsType, devicePath)

	var resizeCmd *exec.Cmd
	switch fsType {
	case "ext2", "ext3", "ext4":
		// For ext filesystems, use resize2fs
		resizeCmd = exec.Command("resize2fs", devicePath)
	case "xfs":
		// For XFS, we need to resize the mounted filesystem
		resizeCmd = exec.Command("xfs_growfs", volumePath)
	default:
		return fmt.Errorf("unsupported filesystem type for resize: %s", fsType)
	}

	if err := resizeCmd.Run(); err != nil {
		return fmt.Errorf("failed to resize %s filesystem on %s: %v", fsType, devicePath, err)
	}

	klog.V(4).Infof("Successfully resized %s filesystem on device %s", fsType, devicePath)
	return nil
}