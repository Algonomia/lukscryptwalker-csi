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
