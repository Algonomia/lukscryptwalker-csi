package driver

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodStuckTerminating(t *testing.T) {
	grace := int64(30)
	mkPod := func(del *time.Time, g *int64) *corev1.Pod {
		p := &corev1.Pod{}
		if del != nil {
			t := metav1.NewTime(*del)
			p.DeletionTimestamp = &t
			p.DeletionGracePeriodSeconds = g
		}
		return p
	}
	now := time.Now()
	past := now.Add(-10 * time.Minute)
	recent := now.Add(-5 * time.Second)

	cases := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"not terminating", mkPod(nil, nil), false},
		{"terminating within grace", mkPod(&recent, &grace), false},
		{"wedged past grace", mkPod(&past, &grace), true},
		{"wedged, nil grace", mkPod(&past, nil), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := podStuckTerminating(c.pod); got != c.want {
				t.Errorf("podStuckTerminating() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestIsMountDeadErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"EIO", &os.PathError{Op: "open", Path: "/x", Err: syscall.EIO}, true},
		{"ENOTCONN", &os.PathError{Op: "stat", Path: "/x", Err: syscall.ENOTCONN}, true},
		{"ESTALE", &os.PathError{Op: "read", Path: "/x", Err: syscall.ESTALE}, true},
		{"ENOENT", &os.PathError{Op: "open", Path: "/x", Err: syscall.ENOENT}, false},
		{"EACCES", &os.PathError{Op: "open", Path: "/x", Err: syscall.EACCES}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isMountDeadErr(c.err); got != c.want {
				t.Errorf("isMountDeadErr(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestProbeMountReads(t *testing.T) {
	t.Run("finds file at depth 2", func(t *testing.T) {
		root := t.TempDir()
		sub := filepath.Join(root, "archive", "db")
		if err := os.MkdirAll(sub, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "archive.info"), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := probeMountReads(root); err != nil {
			t.Errorf("probeMountReads() = %v, want nil", err)
		}
	})

	t.Run("empty volume is healthy", func(t *testing.T) {
		if err := probeMountReads(t.TempDir()); err != nil {
			t.Errorf("probeMountReads() = %v, want nil", err)
		}
	})

	t.Run("dirs only, no regular file", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "a", "b", "c", "d"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := probeMountReads(root); err != nil {
			t.Errorf("probeMountReads() = %v, want nil", err)
		}
	})

	t.Run("missing root is healthy (not a dead-mount errno)", func(t *testing.T) {
		if err := probeMountReads(filepath.Join(t.TempDir(), "gone")); err != nil {
			t.Errorf("probeMountReads() = %v, want nil", err)
		}
	})
}

func TestParseIOPressure(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantPct float64
		want    bool
	}{
		{"stalled node", "some avg10=96.75 avg60=96.49 avg300=96.35 total=3623495088\nfull avg10=87.98 avg60=86.86 avg300=86.77 total=3259639999\n", 87.98, true},
		{"healthy node", "some avg10=0.12 avg60=0.05 avg300=0.01 total=1000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=29\n", 0, false},
		{"at threshold", "full avg10=40.00 avg60=1.00 avg300=1.00 total=1\n", 40.0, true},
		{"garbage", "not psi content", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pct, stalled := parseIOPressure(c.content)
			if stalled != c.want || (c.want && pct != c.wantPct) {
				t.Errorf("parseIOPressure() = (%v, %v), want (%v, %v)", pct, stalled, c.wantPct, c.want)
			}
		})
	}
}

func TestCgroupBelongsToPod(t *testing.T) {
	const uid = "50949d73-f8ea-4bd3-be1b-a395eeec3361"
	cases := []struct {
		name   string
		cgroup string
		want   bool
	}{
		{"systemd style", "0::/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod50949d73_f8ea_4bd3_be1b_a395eeec3361.slice/cri-containerd-abc123.scope", true},
		{"cgroupfs style", "11:memory:/kubepods/besteffort/pod50949d73-f8ea-4bd3-be1b-a395eeec3361/abc123", true},
		{"other pod", "0::/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-podffffffff_f8ea_4bd3_be1b_a395eeec3361.slice/cri.scope", false},
		{"host process", "0::/system.slice/sshd.service", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cgroupBelongsToPod(c.cgroup, uid); got != c.want {
				t.Errorf("cgroupBelongsToPod(%q) = %v, want %v", c.cgroup, got, c.want)
			}
		})
	}
}

func TestVFSRegistryMissing(t *testing.T) {
	const volumeID = "pvc-264e09a8-7045-45ba-9e7e-e325d2f780f2"

	volumeDir := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		body := []byte(`{"volumeHandle":"` + volumeID + `"}`)
		if err := os.WriteFile(filepath.Join(dir, "vol_data.json"), body, 0644); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	newNS := func() *NodeServer { return &NodeServer{s3SyncMgr: NewS3SyncManager()} }

	t.Run("registered VFS is not a zombie", func(t *testing.T) {
		ns := newNS()
		names := map[string]int{volumeID: 1}
		if ns.vfsRegistryMissing(volumeDir(t), names, true) {
			t.Error("a mount whose VFS is registered must not be reported missing")
		}
	})

	t.Run("unreadable registry decides nothing", func(t *testing.T) {
		ns := newNS()
		if ns.vfsRegistryMissing(volumeDir(t), nil, false) {
			t.Error("an unreadable registry must not be read as a missing VFS")
		}
	})

	t.Run("missing VFS needs consecutive confirmations", func(t *testing.T) {
		ns := newNS()
		dir := volumeDir(t)
		for i := 1; i < zombieVFSConfirmations; i++ {
			if ns.vfsRegistryMissing(dir, map[string]int{}, true) {
				t.Fatalf("confirmation %d must not be enough to declare a zombie", i)
			}
		}
		if !ns.vfsRegistryMissing(dir, map[string]int{}, true) {
			t.Errorf("a mount missing from the registry on %d ticks must be reported", zombieVFSConfirmations)
		}
	})

	t.Run("a reappearing VFS clears the strikes", func(t *testing.T) {
		ns := newNS()
		dir := volumeDir(t)
		ns.vfsRegistryMissing(dir, map[string]int{}, true)
		ns.vfsRegistryMissing(dir, map[string]int{volumeID: 1}, true)
		if ns.vfsRegistryMissing(dir, map[string]int{}, true) {
			t.Error("strikes must restart after the VFS is seen again")
		}
	})

	t.Run("a mount still being set up has no VFS yet", func(t *testing.T) {
		ns := newNS()
		ns.s3SyncMgr.markVolumeSetupInProgress(volumeID)
		dir := volumeDir(t)
		for i := 0; i <= zombieVFSConfirmations; i++ {
			if ns.vfsRegistryMissing(dir, map[string]int{}, true) {
				t.Fatal("a volume in setup must never be reported as a zombie")
			}
		}
	})

	t.Run("no vol_data.json", func(t *testing.T) {
		ns := newNS()
		if ns.vfsRegistryMissing(t.TempDir(), map[string]int{}, true) {
			t.Error("an unidentifiable volume must not be reported as a zombie")
		}
	})
}
