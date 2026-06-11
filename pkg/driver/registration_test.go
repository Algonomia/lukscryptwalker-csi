package driver

import (
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestIsKubeletCmdline(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"plain kubelet", []string{"/usr/bin/kubelet", "--config=/var/lib/kubelet/config.yaml"}, true},
		{"k3s server", []string{"/usr/local/bin/k3s", "server"}, true},
		{"k3s agent", []string{"/usr/local/bin/k3s", "agent"}, true},
		{"k3s kubectl", []string{"/usr/local/bin/k3s", "kubectl", "get", "pods"}, false},
		{"k3s ctr", []string{"k3s", "ctr"}, false},
		{"k3s bare", []string{"k3s"}, false},
		{"rke2 server", []string{"/usr/local/bin/rke2", "server"}, true},
		{"rke2-agent wrapper", []string{"/usr/local/bin/rke2-agent", "agent"}, true},
		{"containerd", []string{"/usr/bin/containerd"}, false},
		{"kubelet-lookalike path", []string{"/opt/kubelet/run.sh"}, false},
		{"empty cmdline", []string{""}, false},
		{"no args", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isKubeletCmdline(c.args); got != c.want {
				t.Errorf("isKubeletCmdline(%v) = %v, want %v", c.args, got, c.want)
			}
		})
	}
}

func TestPidListeningOn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	pid, ok := pidListeningOn(port)
	if !ok {
		t.Fatalf("did not find listener on port %d", port)
	}
	if pid != strconv.Itoa(os.Getpid()) {
		t.Errorf("pidListeningOn(%d) = %s, want %d (self)", port, pid, os.Getpid())
	}

	_ = ln.Close()
	if _, ok := pidListeningOn(port); ok {
		t.Errorf("found listener on closed port %d", port)
	}
}

func TestProcessStartTicks(t *testing.T) {
	ticks, ok := processStartTicks("self")
	if !ok {
		t.Fatal("processStartTicks(self) failed")
	}
	if ticks <= 0 {
		t.Errorf("expected positive starttime ticks, got %d", ticks)
	}

	if _, ok := processStartTicks("not-a-pid"); ok {
		t.Error("expected failure for nonexistent pid")
	}
}

func TestHostBootTime(t *testing.T) {
	boot, ok := hostBootTime()
	if !ok {
		t.Fatal("hostBootTime failed")
	}
	if boot.After(time.Now()) {
		t.Errorf("boot time %s is in the future", boot)
	}
	if boot.Before(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("boot time %s is implausibly old", boot)
	}
}

func TestKubeletProcessStartConsistency(t *testing.T) {
	// On a dev machine a kubelet may or may not be running; only validate
	// that a reported start time is sane.
	start, ok := kubeletProcessStart()
	if ok && (start.After(time.Now()) || start.IsZero()) {
		t.Errorf("kubelet start time %s is not sane", start)
	}
}
