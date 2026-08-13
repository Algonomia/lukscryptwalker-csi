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
