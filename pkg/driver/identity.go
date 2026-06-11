package driver

import (
	"context"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/lukscryptwalker-csi/pkg/rclone"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
)

type IdentityServer struct {
	csi.UnimplementedIdentityServer
	driver *Driver
}

func NewIdentityServer(d *Driver) *IdentityServer {
	return &IdentityServer{
		driver: d,
	}
}

func (ids *IdentityServer) GetPluginInfo(ctx context.Context, req *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	klog.Infof("GetPluginInfo called")

	if ids.driver.name == "" {
		return nil, status.Error(codes.Unavailable, "Driver name not configured")
	}

	if ids.driver.version == "" {
		return nil, status.Error(codes.Unavailable, "Driver version not configured")
	}

	return &csi.GetPluginInfoResponse{
		Name:          ids.driver.name,
		VendorVersion: ids.driver.version,
	}, nil
}

// Probe verifies librclone still answers: a hung rclone means mounts and S3
// operations are dead even while the gRPC server keeps responding.
func (ids *IdentityServer) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	klog.V(5).Info("Probe called")

	done := make(chan error, 1)
	go func() {
		_, err := rclone.RPC("core/version", map[string]interface{}{})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "librclone unhealthy: %v", err)
		}
	case <-time.After(5 * time.Second):
		return nil, status.Error(codes.FailedPrecondition, "librclone unresponsive")
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	return &csi.ProbeResponse{}, nil
}

func (ids *IdentityServer) GetPluginCapabilities(ctx context.Context, req *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	klog.Infof("GetPluginCapabilities called")

	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
		},
	}, nil
}