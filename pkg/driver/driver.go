package driver

import (
	"context"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
)

const (
	DriverName    = "lukscryptwalker.csi.k8s.io"
	DriverVersion = "1.0.6"
)

type Driver struct {
	name     string
	version  string
	nodeID   string
	endpoint string
	srv      *grpc.Server

	ids *IdentityServer
	ns  *NodeServer
	cs  *ControllerServer
}

func GetVersion() string {
	return DriverVersion
}

func NewDriver(endpoint, nodeID string) *Driver {
	klog.Infof("Driver: %v version: %v", DriverName, DriverVersion)

	d := &Driver{
		name:     DriverName,
		version:  DriverVersion,
		nodeID:   nodeID,
		endpoint: endpoint,
	}

	d.ids = NewIdentityServer(d)
	d.ns = NewNodeServer(d)
	d.cs = NewControllerServer(d)

	return d
}

func (d *Driver) Run(ctx context.Context) error {
	u, err := url.Parse(d.endpoint)
	if err != nil {
		return err
	}

	addr := path.Join(u.Host, filepath.FromSlash(u.Path))
	if u.Host == "" {
		addr = filepath.FromSlash(u.Path)
	}

	// Remove socket file if it exists
	if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
		return err
	}

	listener, err := net.Listen(u.Scheme, addr)
	if err != nil {
		return err
	}

	d.srv = grpc.NewServer()

	// Register CSI services
	csi.RegisterIdentityServer(d.srv, d.ids)
	csi.RegisterNodeServer(d.srv, d.ns)
	csi.RegisterControllerServer(d.srv, d.cs)

	klog.Infof("Listening for connections on address: %#v", listener.Addr())

	errChan := make(chan error, 1)
	go func() {
		errChan <- d.srv.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		klog.Info("Server context cancelled, stopping gracefully")
		d.srv.GracefulStop()
		return nil
	case err := <-errChan:
		return err
	}
}

func (d *Driver) GetName() string {
	return d.name
}

func (d *Driver) GetVersion() string {
	return d.version
}

func (d *Driver) GetNodeID() string {
	return d.nodeID
}
