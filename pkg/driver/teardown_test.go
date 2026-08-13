package driver

import (
	"reflect"
	"testing"
)

// A backing file whose loop device outlived its mapper keeps 60G allocated even
// after the file is deleted, so the deleted-marker form has to parse too.
func TestParseLosetupJ(t *testing.T) {
	const out = `/dev/loop3: [64769]:1310721 (/mnt/encrypted-volumes/pvc-a/luks-pvc-a.img)
/dev/loop7: [64769]:1310722 (/mnt/encrypted-volumes/pvc-a/luks-pvc-a.img (deleted))
`
	got := parseLosetupJ(out)
	want := []string{"/dev/loop3", "/dev/loop7"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseLosetupJ = %v, want %v", got, want)
	}

	if got := parseLosetupJ(""); got != nil {
		t.Errorf("empty output produced %v, want nil", got)
	}
	// losetup prints nothing but a warning line when the file has no loop.
	if got := parseLosetupJ("losetup: /nope: No such file or directory\n"); got != nil {
		t.Errorf("non-device output produced %v, want nil", got)
	}
}

// Binds must be released before the mount they point at, or the umount fails
// busy and the mapper can never be closed.
func TestParseFindmntTargetsOrdersDeepestFirst(t *testing.T) {
	const out = `/var/snap/microk8s/common/var/lib/kubelet/plugins/kubernetes.io/csi/lukscrypt/abc/globalmount
/var/snap/microk8s/common/var/lib/kubelet/pods/uid-1/volumes/kubernetes.io~csi/pvc-a/mount
`
	got := parseFindmntTargets(out)
	if len(got) != 2 {
		t.Fatalf("got %d targets, want 2: %v", len(got), got)
	}
	if len(got[0]) < len(got[1]) {
		t.Errorf("targets are not deepest-first: %v", got)
	}
}

func TestParseFindmntTargetsUnescapes(t *testing.T) {
	got := parseFindmntTargets(`/mnt/with\x20space`)
	want := []string{"/mnt/with space"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseFindmntTargets = %q, want %q", got, want)
	}
}
