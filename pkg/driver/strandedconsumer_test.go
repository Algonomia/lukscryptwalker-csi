package driver

import (
	"testing"

	"github.com/lukscryptwalker-csi/pkg/rclone"
)

const testKubeletRoot = "/var/lib/kubelet"

func fuseMount(dev string) rclone.HostMount { return rclone.HostMount{Dev: dev, FSType: "fuse.rclone"} }

func bindPath(pvName string) string {
	return testKubeletRoot + "/pods/pod-uid-1/volumes/kubernetes.io~csi/" + pvName + "/mount"
}

// A stale bind buried under a fresh one leaves a container reading EIO from a
// shut-down VFS while every path-based check passes. Both mounts are
// fuse.rclone binds of the same volume, so only the devices tell them apart.
func TestParseConsumerBindsRecordsDepthAndLiveDevice(t *testing.T) {
	path := bindPath("pvc-1")
	stacks := map[string][]rclone.HostMount{
		path: {fuseMount("0:274"), fuseMount("0:1320")},
	}

	got := parseConsumerBinds(stacks, testKubeletRoot)
	if len(got) != 1 {
		t.Fatalf("found %d consumer binds, want 1: %+v", len(got), got)
	}
	b := got[0]
	if b.podUID != "pod-uid-1" || b.pvName != "pvc-1" {
		t.Errorf("parsed podUID=%q pvName=%q, want pod-uid-1/pvc-1", b.podUID, b.pvName)
	}
	if b.depth != 2 {
		t.Errorf("depth = %d, want 2", b.depth)
	}
	if b.dev != "0:1320" {
		t.Errorf("live device = %q, want 0:1320 (the topmost)", b.dev)
	}
}

// The signal that matters is not "stacked" but "the host no longer resolves
// it". A re-bind that correctly removes the old mount still leaves the
// container on the old superblock, with nothing stacked to notice.
func TestLiveMountDevicesExcludesBuriedAndDeparted(t *testing.T) {
	stacks := map[string][]rclone.HostMount{
		bindPath("pvc-1"): {fuseMount("0:274"), fuseMount("0:1320")},
		bindPath("pvc-2"): {fuseMount("0:815")},
	}

	live := liveMountDevices(stacks)
	if !live["0:1320"] || !live["0:815"] {
		t.Errorf("topmost devices missing from the live set: %v", live)
	}
	if live["0:274"] {
		t.Error("a buried device was reported live")
	}
	// The case the kind run surfaced: a container holding 0:816 after the bind
	// moved to 0:815, with nothing stacked anywhere.
	if live["0:816"] {
		t.Error("a device absent from the table was reported live")
	}
}

// Repair restarts pods, so anything that is not unambiguously one of our
// consumer binds must be left alone.
func TestParseConsumerBindsIgnoresEverythingElse(t *testing.T) {
	cases := map[string]struct {
		path   string
		mounts []rclone.HostMount
	}{
		"another driver's non-FUSE volume": {
			bindPath("pvc-2"),
			[]rclone.HostMount{{Dev: "8:1", FSType: "ext4"}, {Dev: "8:2", FSType: "ext4"}},
		},
		"subPath below the bind": {
			bindPath("pvc-3") + "/sub",
			[]rclone.HostMount{fuseMount("0:200"), fuseMount("0:201")},
		},
		"staging mount, not a consumer bind": {
			testKubeletRoot + "/plugins/kubernetes.io/csi/" + DriverName + "/hash/globalmount",
			[]rclone.HostMount{fuseMount("0:300")},
		},
		"a different kubelet root": {
			"/var/snap/microk8s/common/var/lib/kubelet/pods/uid/volumes/kubernetes.io~csi/pvc-4/mount",
			[]rclone.HostMount{fuseMount("0:400")},
		},
		"in-tree volume plugin": {
			testKubeletRoot + "/pods/uid/volumes/kubernetes.io~empty-dir/cache/mount",
			[]rclone.HostMount{fuseMount("0:500")},
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := parseConsumerBinds(map[string][]rclone.HostMount{c.path: c.mounts}, testKubeletRoot)
			if len(got) != 0 {
				t.Errorf("%s was picked up for repair: %+v", name, got)
			}
		})
	}
}

// A healthy bind must produce no work at all, or the sweep would churn every
// consumer on the node every five minutes.
func TestHealthyBindIsNotRepairable(t *testing.T) {
	path := bindPath("pvc-1")
	stacks := map[string][]rclone.HostMount{path: {fuseMount("0:815")}}

	binds := parseConsumerBinds(stacks, testKubeletRoot)
	if len(binds) != 1 {
		t.Fatalf("found %d consumer binds, want 1", len(binds))
	}
	if binds[0].depth != 1 {
		t.Errorf("depth = %d, want 1", binds[0].depth)
	}
	// A container on the same device is not stranded, so with depth 1 there is
	// nothing for the sweep to do.
	if live := liveMountDevices(stacks); !live[binds[0].dev] {
		t.Error("the live device of a healthy bind was not in the live set")
	}
}
