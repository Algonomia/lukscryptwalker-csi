package driver

import (
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestEndpointAddr(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		want     string
	}{
		{"node endpoint", "unix:///csi/csi.sock", "/csi/csi.sock"},
		{"controller endpoint", "unix:///var/lib/csi/sockets/pluginproxy/csi.sock", "/var/lib/csi/sockets/pluginproxy/csi.sock"},
		{"bare path", "/tmp/csi.sock", "/tmp/csi.sock"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := endpointAddr(c.endpoint); got != c.want {
				t.Errorf("endpointAddr(%q) = %q, want %q", c.endpoint, got, c.want)
			}
		})
	}
}

// A wedged driver keeps its listener bound but never accepts; an unroutable
// endpoint must read as not-accepting so the registrar is spared.
func TestCsiSocketAccepting(t *testing.T) {
	sock := t.TempDir() + "/csi.sock"

	ns := &NodeServer{driver: &Driver{endpoint: "unix://" + sock}}
	if ns.csiSocketAccepting() {
		t.Error("expected not-accepting when no socket exists")
	}

	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if !ns.csiSocketAccepting() {
		t.Error("expected accepting when a listener is bound")
	}

	// Unknown endpoint must fail open rather than punish the registrar.
	nsUnknown := &NodeServer{driver: &Driver{endpoint: ""}}
	if !nsUnknown.csiSocketAccepting() {
		t.Error("expected fail-open on an unparseable endpoint")
	}
}

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
	defer func() { _ = ln.Close() }()
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

// The controller pods construct a NodeServer too; node-only background work
// (mount checker, host watchdog, FUSE aborts) must never run there.
func TestIsNodeMode(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		want     bool
	}{
		{"node daemonset", "unix:///csi/csi.sock", true},
		{"controller deployment", "unix:///var/lib/csi/sockets/pluginproxy/csi.sock", false},
		{"bare node path", "/csi/csi.sock", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &Driver{endpoint: c.endpoint}
			if got := d.IsNodeMode(); got != c.want {
				t.Errorf("IsNodeMode(%q) = %v, want %v", c.endpoint, got, c.want)
			}
		})
	}
}

func TestConsumerRestartAllowedGatedOnRegistration(t *testing.T) {
	ns := &NodeServer{}
	if !ns.consumerRestartAllowed("vol-1") {
		t.Fatal("first call on a registered driver must be allowed")
	}
	if ns.consumerRestartAllowed("vol-1") {
		t.Error("second call within the cooldown must be refused")
	}

	ns.regUnhealthy.Store(true)
	if ns.consumerRestartAllowed("vol-2") {
		t.Error("an unregistered driver must not destructively recover consumers")
	}
	// The refusal must not have stamped the cooldown, or recovery would stay
	// blocked for another 5 minutes after registration comes back.
	ns.regUnhealthy.Store(false)
	if !ns.consumerRestartAllowed("vol-2") {
		t.Error("recovery must be allowed again as soon as registration returns")
	}
}

func TestReconcileBackoff(t *testing.T) {
	ns := &NodeServer{}

	// A one-off repair is never delayed: the free attempts, then a reset the
	// moment a tick sees the mount healthy.
	for i := 0; i < reconcileFreeAttempts; i++ {
		if !ns.reconcileAllowed("vol-1") {
			t.Fatalf("attempt %d must be allowed before backoff starts", i+1)
		}
	}
	if ns.reconcileAllowed("vol-1") {
		t.Error("attempt past the free budget must back off")
	}
	ns.reconcileSucceeded("vol-1")
	if !ns.reconcileAllowed("vol-1") {
		t.Error("a healthy observation must clear the backoff")
	}

	// The wait grows and is capped.
	ns.reconcileAttempts.Store("vol-2", reconcileAttempt{count: 99, last: time.Now()})
	if ns.reconcileAllowed("vol-2") {
		t.Error("a long-failing volume must still be backed off")
	}
	ns.reconcileAttempts.Store("vol-2", reconcileAttempt{
		count: 99, last: time.Now().Add(-reconcileBackoffMax - time.Second)})
	if !ns.reconcileAllowed("vol-2") {
		t.Error("backoff must never exceed reconcileBackoffMax")
	}
}
