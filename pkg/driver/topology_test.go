package driver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation"
)

func nodeTopo(node string) *csi.Topology {
	return &csi.Topology{Segments: map[string]string{TopologyKeyNode: node}}
}

func createReq(params map[string]string, topo *csi.TopologyRequirement) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name: "pvc-1",
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		}},
		Parameters:                params,
		AccessibilityRequirements: topo,
	}
}

// A LUKS volume must come back pinned to the scheduler-selected node, or a
// reschedule strands its data.
func TestCreateVolumePinsLUKSVolumeToPreferredNode(t *testing.T) {
	cs := &ControllerServer{}
	resp, err := cs.CreateVolume(context.Background(), createReq(nil, &csi.TopologyRequirement{
		Requisite: []*csi.Topology{nodeTopo("node-b"), nodeTopo("node-a")},
		Preferred: []*csi.Topology{nodeTopo("node-a"), nodeTopo("node-b")},
	}))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	at := resp.GetVolume().GetAccessibleTopology()
	if len(at) != 1 || at[0].GetSegments()[TopologyKeyNode] != "node-a" {
		t.Fatalf("AccessibleTopology = %v, want only node-a", at)
	}
	if got := resp.GetVolume().GetVolumeContext()[PinnedNodeParam]; got != "node-a" {
		t.Fatalf("volume context %s = %q, want node-a", PinnedNodeParam, got)
	}
}

func TestCreateVolumeLUKSWithoutTopologyFails(t *testing.T) {
	cs := &ControllerServer{}
	_, err := cs.CreateVolume(context.Background(), createReq(nil, nil))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
}

func TestCreateVolumeS3IsNotPinned(t *testing.T) {
	cs := &ControllerServer{}
	resp, err := cs.CreateVolume(context.Background(), createReq(
		map[string]string{StorageBackendParam: "s3"},
		&csi.TopologyRequirement{Preferred: []*csi.Topology{nodeTopo("node-a")}},
	))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if at := resp.GetVolume().GetAccessibleTopology(); len(at) != 0 {
		t.Fatalf("S3 volume pinned to %v; it is reachable from every node", at)
	}
	if _, ok := resp.GetVolume().GetVolumeContext()[PinnedNodeParam]; ok {
		t.Fatalf("S3 volume context carries %s", PinnedNodeParam)
	}
}

func TestNodeGetInfoReportsTopology(t *testing.T) {
	ns := &NodeServer{driver: &Driver{nodeID: "node-a"}}
	resp, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo: %v", err)
	}
	if got := resp.GetAccessibleTopology().GetSegments()[TopologyKeyNode]; got != "node-a" {
		t.Fatalf("topology segment = %q, want node-a", got)
	}
}

// Staging a pinned volume on another node must not create an empty container
// in place of the real one.
func TestEnsureBackingFileRefusesOffPinnedNode(t *testing.T) {
	ns := &NodeServer{driver: &Driver{nodeID: "node-b"}}
	backing := filepath.Join(t.TempDir(), "pvc-1.img")
	req := &csi.NodeStageVolumeRequest{VolumeContext: map[string]string{
		"capacity":      "33554432",
		PinnedNodeParam: "node-a",
	}}

	err := ns.ensureBackingFileExists(req, backing)
	if err == nil || !strings.Contains(err.Error(), "pinned to node node-a") {
		t.Fatalf("err = %v, want refusal naming node-a", err)
	}
	if _, statErr := os.Stat(backing); !os.IsNotExist(statErr) {
		t.Fatalf("backing file was created off the pinned node")
	}
}

// The registrar writes TopologyKeyNode as a node label through the kubelet's
// credentials; NodeRestriction rejects k8s.io/kubernetes.io prefixes.
func TestTopologyKeyIsKubeletSettableLabel(t *testing.T) {
	if errs := validation.IsQualifiedName(TopologyKeyNode); len(errs) > 0 {
		t.Fatalf("%q is not a valid label key: %v", TopologyKeyNode, errs)
	}
	prefix, _, _ := strings.Cut(TopologyKeyNode, "/")
	for _, ns := range []string{"k8s.io", "kubernetes.io"} {
		if prefix == ns || strings.HasSuffix(prefix, "."+ns) {
			t.Fatalf("%q is in the %s namespace; NodeRestriction forbids kubelets to set it", TopologyKeyNode, ns)
		}
	}
}
