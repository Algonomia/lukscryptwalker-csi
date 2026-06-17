package driver

import (
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
