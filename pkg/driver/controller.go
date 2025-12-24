package driver

import (
	"context"
	"fmt"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/lukscryptwalker-csi/pkg/rclone"
	"github.com/lukscryptwalker-csi/pkg/secrets"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
)

type ControllerServer struct {
	driver *Driver
}

func NewControllerServer(d *Driver) *ControllerServer {
	return &ControllerServer{
		driver: d,
	}
}

func (cs *ControllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	klog.Infof("CreateVolume called with request: %+v", req)

	name := req.GetName()
	if len(name) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Name missing in request")
	}

	caps := req.GetVolumeCapabilities()
	if caps == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities missing in request")
	}

	// Validate volume capabilities
	if err := cs.validateVolumeCapabilities(caps); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Invalid volume capabilities: %v", err)
	}

	// Get storage requirements
	capacityBytes := int64(1 * 1024 * 1024 * 1024) // Default 1GB
	if req.GetCapacityRange() != nil {
		capacityBytes = req.GetCapacityRange().GetRequiredBytes()
		if capacityBytes == 0 {
			capacityBytes = req.GetCapacityRange().GetLimitBytes()
		}
		if capacityBytes == 0 {
			capacityBytes = int64(1 * 1024 * 1024 * 1024) // Default 1GB
		}
	}

	// Store volume metadata - nodes will handle directory creation
	volumeContext := map[string]string{
		"capacity": fmt.Sprintf("%d", capacityBytes),
	}

	// Add any additional parameters to volume context
	for k, v := range req.GetParameters() {
		volumeContext[k] = v
	}

	volume := &csi.Volume{
		VolumeId:      name,
		CapacityBytes: capacityBytes,
		VolumeContext: volumeContext,
	}

	klog.Infof("Created volume: %+v", volume)
	return &csi.CreateVolumeResponse{Volume: volume}, nil
}

func (cs *ControllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	klog.Infof("DeleteVolume called with request: %+v", req)

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	// Check if this is an S3 volume by looking at secrets
	reqSecrets := req.GetSecrets()
	klog.V(4).Infof("DeleteVolume secrets: %v", reqSecrets)

	if reqSecrets == nil || len(reqSecrets) == 0 {
		klog.Infof("No secrets provided to DeleteVolume - S3 data will not be deleted. Configure provisioner-secret-name/namespace in StorageClass for ReclaimPolicy:Delete to work with S3.")
	} else {
		// Use the secrets helper to extract S3 configuration
		s3Creds := secrets.S3ConfigFromSecrets(reqSecrets)
		if s3Creds == nil {
			klog.Infof("No S3 credentials found in secrets (keys: %v) - not an S3 volume or missing credentials", getSecretKeys(reqSecrets))
		} else {
			klog.Infof("Deleting S3 data for volume: %s", volumeID)

			s3Config := &rclone.S3Config{
				Region:          s3Creds.Region,
				Endpoint:        s3Creds.Endpoint,
				AccessKeyID:     s3Creds.AccessKeyID,
				SecretAccessKey: s3Creds.SecretAccessKey,
				Bucket:          s3Creds.Bucket,
			}

			if err := rclone.DeleteVolumeData(s3Config, volumeID, s3Creds.PathPrefix); err != nil {
				klog.Errorf("Failed to delete S3 data for volume %s: %v", volumeID, err)
				// Don't fail the deletion - the volume should still be removed from Kubernetes
				// The S3 data might need manual cleanup if this fails
			}
		}
	}

	klog.Infof("Controller completed deletion of volume: %s", volumeID)
	return &csi.DeleteVolumeResponse{}, nil
}

// getSecretKeys returns the keys from a secrets map (for logging, without exposing values)
func getSecretKeys(secrets map[string]string) []string {
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		keys = append(keys, k)
	}
	return keys
}

func (cs *ControllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ControllerPublishVolume is not implemented")
}

func (cs *ControllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ControllerUnpublishVolume is not implemented")
}

func (cs *ControllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	caps := req.GetVolumeCapabilities()
	if caps == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities missing in request")
	}

	// Validate the capabilities
	if err := cs.validateVolumeCapabilities(caps); err != nil {
		return &csi.ValidateVolumeCapabilitiesResponse{
			Message: err.Error(),
		}, nil
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: caps,
		},
	}, nil
}

func (cs *ControllerServer) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ListVolumes is not implemented")
}

func (cs *ControllerServer) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	return nil, status.Error(codes.Unimplemented, "GetCapacity is not implemented")
}

func (cs *ControllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
					},
				},
			},
			{
				Type: &csi.ControllerServiceCapability_Rpc{
					Rpc: &csi.ControllerServiceCapability_RPC{
						Type: csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
					},
				},
			},
		},
	}, nil
}

func (cs *ControllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "CreateSnapshot is not implemented")
}

func (cs *ControllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "DeleteSnapshot is not implemented")
}

func (cs *ControllerServer) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ListSnapshots is not implemented")
}

func (cs *ControllerServer) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	klog.Infof("ControllerExpandVolume called with request: %+v", req)

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	capacityRange := req.GetCapacityRange()
	if capacityRange == nil {
		return nil, status.Error(codes.InvalidArgument, "Capacity range missing in request")
	}

	requestedBytes := capacityRange.GetRequiredBytes()
	if requestedBytes == 0 {
		requestedBytes = capacityRange.GetLimitBytes()
	}
	if requestedBytes == 0 {
		return nil, status.Error(codes.InvalidArgument, "Required bytes missing in capacity range")
	}

	klog.Infof("Controller approving expansion of volume %s to %d bytes", volumeID, requestedBytes)

	// Controller's job: Validate the expansion request and delegate to node
	// The node will handle the actual backing file expansion, LUKS resize, and filesystem resize
	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         requestedBytes,
		NodeExpansionRequired: true, // Node service will handle all local operations
	}, nil
}

func (cs *ControllerServer) ControllerGetVolume(ctx context.Context, req *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ControllerGetVolume is not implemented")
}

func (cs *ControllerServer) validateVolumeCapabilities(caps []*csi.VolumeCapability) error {
	for _, cap := range caps {
		// Only support mount access type
		if cap.GetMount() == nil {
			return fmt.Errorf("only mount access type is supported")
		}

		// Validate access mode
		accessMode := cap.GetAccessMode().GetMode()
		if accessMode != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER &&
			accessMode != csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY {
			return fmt.Errorf("unsupported access mode: %v", accessMode)
		}

		// Validate filesystem type
		mount := cap.GetMount()
		fsType := mount.GetFsType()
		if fsType != "" && fsType != "ext2" && fsType != "ext3" && fsType != "ext4" && fsType != "xfs" {
			return fmt.Errorf("unsupported filesystem type: %s", fsType)
		}
	}

	return nil
}
