package driver

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/lukscryptwalker-csi/pkg/rclone"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog"
)

// librcloneHangThreshold is how long librclone must be CONTINUOUSLY
// unresponsive before Probe reports failure. A single slow call means busy —
// the startup reconcile mounts every volume at once — and failing on that
// makes kubelet kill the driver mid-reconcile, which leaves a zombie sandbox
// and restarts the same overload. Only a sustained silence means hung.
const librcloneHangThreshold = 2 * time.Minute

// lastLibrcloneOK is the last time librclone answered, as UnixNano.
var lastLibrcloneOK atomic.Int64

func init() { lastLibrcloneOK.Store(time.Now().UnixNano()) }

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
// operations are dead even while the gRPC server keeps responding. Reports
// failure only once librclone has been unresponsive for librcloneHangThreshold
// — killing the driver over one slow call costs every mount on the node.
func (ids *IdentityServer) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	klog.V(5).Info("Probe called")

	done := make(chan error, 1)
	go func() {
		_, err := rclone.RPC("core/version", map[string]interface{}{})
		done <- err
	}()

	var reason string
	select {
	case err := <-done:
		if err == nil {
			lastLibrcloneOK.Store(time.Now().UnixNano())
			return &csi.ProbeResponse{}, nil
		}
		reason = "librclone unhealthy: " + err.Error()
	case <-time.After(5 * time.Second):
		reason = "librclone did not answer within 5s"
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}

	silent := time.Since(time.Unix(0, lastLibrcloneOK.Load()))
	if silent < librcloneHangThreshold {
		klog.Warningf("Probe: %s (last answered %s ago) — reporting healthy: under sustained load "+
			"librclone is slow, not hung, and a kill here would tear down every mount on this node",
			reason, silent.Round(time.Second))
		return &csi.ProbeResponse{}, nil
	}
	klog.Errorf("Probe: %s and librclone has been unresponsive for %s — reporting unhealthy",
		reason, silent.Round(time.Second))
	return nil, status.Errorf(codes.FailedPrecondition, "%s (unresponsive for %s)", reason, silent.Round(time.Second))
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