package driver

import "testing"

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
