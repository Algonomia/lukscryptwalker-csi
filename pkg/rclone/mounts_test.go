package rclone

import (
	"os"
	"testing"
)

func TestParseMountInfo(t *testing.T) {
	const data = `25 30 0:23 / /sys rw,nosuid,relatime shared:7 - sysfs sysfs rw
30 1 8:1 / / rw,relatime - ext4 /dev/sda1 rw,discard
812 30 0:145 / /var/lib/kubelet/plugins/kubernetes.io/csi/lukscryptwalker.csi.k8s.io/abc/globalmount rw,nosuid,nodev,relatime shared:401 - fuse.rclone pvc-1234: rw,user_id=0,group_id=0
900 30 8:1 /data /mnt/with\040space rw,relatime - ext4 /dev/sda1 rw
truncated line`

	got := parseMountInfo(data)

	cases := []struct {
		path   string
		fsType string
	}{
		{"/sys", "sysfs"},
		{"/", "ext4"},
		{"/var/lib/kubelet/plugins/kubernetes.io/csi/lukscryptwalker.csi.k8s.io/abc/globalmount", "fuse.rclone"},
		{"/mnt/with space", "ext4"}, // octal escape decoded
	}
	for _, c := range cases {
		if got[c.path] != c.fsType {
			t.Errorf("parseMountInfo()[%q] = %q, want %q", c.path, got[c.path], c.fsType)
		}
	}
	if len(got) != len(cases) {
		t.Errorf("parsed %d mounts, want %d: %v", len(got), len(cases), got)
	}
}

// The whole point of reading the host table: an ext4 mount sitting where a
// FUSE mount belongs means the volume is dead and the bind is exposing the
// unencrypted directory underneath — it must never read as a healthy mount.
func TestFUSEDetectionRejectsShadowedMount(t *testing.T) {
	mounts := parseMountInfo(
		`812 30 8:1 / /var/lib/kubelet/plugins/kubernetes.io/csi/d/globalmount rw,relatime - ext4 /dev/sda1 rw`)

	fsType, ok := mounts["/var/lib/kubelet/plugins/kubernetes.io/csi/d/globalmount"]
	if !ok {
		t.Fatal("mount not parsed")
	}
	if fsType == "fuse.rclone" {
		t.Fatalf("ext4 shadow mount reported as FUSE")
	}
}

// A rebind that fails to remove the old mount stacks a fresh bind on a stale
// one. stat() and the flattened table both show only the top, so the buried
// mount — still serving containers that opened it earlier — must be visible
// through the stack view or nothing can ever detect it.
func TestStackedMountsAreNotCollapsed(t *testing.T) {
	const path = "/var/lib/kubelet/pods/uid/volumes/kubernetes.io~csi/pvc-1/mount"
	data := `600 30 0:274 / ` + path + ` rw,relatime shared:1 - fuse.rclone pvc-1: rw
900 30 0:1320 / ` + path + ` rw,relatime shared:2 - fuse.rclone pvc-1: rw`

	stacks := parseMountStacks(data)
	if got := len(stacks[path]); got != 2 {
		t.Fatalf("parseMountStacks found %d mounts at the path, want 2", got)
	}
	// Both are fuse.rclone binds of the same volume; only the device tells the
	// live mount from the shut-down one the container may still be holding.
	if got, want := stacks[path][0].Dev, "0:274"; got != want {
		t.Errorf("buried mount device = %q, want %q", got, want)
	}
	if got, want := stacks[path][1].Dev, "0:1320"; got != want {
		t.Errorf("topmost mount device = %q, want %q", got, want)
	}
	if got := len(parseMountInfo(data)); got != 1 {
		t.Errorf("flattened table has %d entries, want 1 (topmost only)", got)
	}

	pinMountsStacks(t, stacks)
	depth, known := HostMountDepth(path)
	if !known {
		t.Fatal("depth reported as unknown from a readable table")
	}
	if depth != 2 {
		t.Errorf("HostMountDepth = %d, want 2", depth)
	}
	if depth, _ := HostMountDepth("/not/mounted"); depth != 0 {
		t.Errorf("HostMountDepth of an unmounted path = %d, want 0", depth)
	}
}

// A node rename must not orphan the cache: the passphrase embeds the node id,
// so the id recorded at format time wins over the current one.
func TestVFSCachePassphrase(t *testing.T) {
	// No marker (fresh install or pre-existing deployment): use the current id.
	if got, want := vfsCachePassphrase("secret", "node-a"), "secret-node-a"; got != want {
		t.Errorf("without marker = %q, want %q", got, want)
	}

	dir := t.TempDir()
	orig := vfsCacheNodeIDFile
	vfsCacheNodeIDFile = dir + "/.node-id"
	defer func() { vfsCacheNodeIDFile = orig }()

	if err := os.WriteFile(vfsCacheNodeIDFile, []byte("node-a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Node renamed to node-b: still derive from the recorded node-a.
	if got, want := vfsCachePassphrase("secret", "node-b"), "secret-node-a"; got != want {
		t.Errorf("after rename = %q, want %q", got, want)
	}
	// Same node: unchanged.
	if got, want := vfsCachePassphrase("secret", "node-a"), "secret-node-a"; got != want {
		t.Errorf("same node = %q, want %q", got, want)
	}
}
