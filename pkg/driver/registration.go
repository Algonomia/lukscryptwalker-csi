package driver

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
)

// RegistrationHealthPort is where the driver serves its registration-health
// endpoint; the node-driver-registrar's liveness probe targets it so a lost
// registration restarts only the registrar, not the driver or its mounts.
const RegistrationHealthPort = ":9810"

// registrationStartupGrace lets initial registration complete before the
// endpoint can report unhealthy.
const registrationStartupGrace = 90 * time.Second

// kubeletRehandshakeGrace is how long after a kubelet (re)start we wait for
// it to re-register the driver before reporting unhealthy.
const kubeletRehandshakeGrace = 90 * time.Second

var registrationHealthStart time.Time

// RunRegistrationHealthServer serves GET /healthz: 200 while the driver is in
// this node's CSINode, 503 otherwise. Blocks; run in a goroutine.
func (ns *NodeServer) RunRegistrationHealthServer(addr string) {
	registrationHealthStart = time.Now()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		healthy := ns.isDriverRegistered()
		ns.recordRegistrationTransition(healthy)
		if healthy {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("registered"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("driver not registered with kubelet"))
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	klog.Infof("Registration health server listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil {
		klog.Errorf("Registration health server stopped: %v", err)
	}
}

// recordRegistrationTransition emits a Node event when registration health
// changes state, so a lost registration is visible without reading driver logs.
func (ns *NodeServer) recordRegistrationTransition(healthy bool) {
	was := ns.regUnhealthy.Swap(!healthy)
	if ns.recorder == nil || was == !healthy {
		return
	}
	nodeRef := &corev1.ObjectReference{Kind: "Node", Name: ns.driver.nodeID, UID: types.UID(ns.driver.nodeID)}
	if healthy {
		ns.recorder.Eventf(nodeRef, corev1.EventTypeNormal, "CSIRegistrationRecovered",
			"CSI driver %s re-registered with kubelet", DriverName)
	} else {
		ns.recorder.Eventf(nodeRef, corev1.EventTypeWarning, "CSIRegistrationLost",
			"CSI driver %s is not registered with kubelet; restarting the registrar to force re-registration", DriverName)
	}
}

// isDriverRegistered reports whether kubelet currently knows about the driver,
// failing open during startup grace and on errors so the registrar is
// restarted only on a confirmed absence.
func (ns *NodeServer) isDriverRegistered() bool {
	if time.Since(registrationHealthStart) < registrationStartupGrace {
		return true
	}
	if ns.clientset == nil || ns.driver.nodeID == "" {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	csiNode, err := ns.clientset.StorageV1().CSINodes().Get(ctx, ns.driver.nodeID, metav1.GetOptions{})
	if err != nil {
		klog.V(4).Infof("registration check: could not get CSINode %s: %v (failing open)", ns.driver.nodeID, err)
		return true
	}
	found := false
	for _, d := range csiNode.Spec.Drivers {
		if d.Name == DriverName {
			found = true
			break
		}
	}
	if !found {
		klog.Warningf("registration check: driver %s is NOT present in CSINode %s — "+
			"registrar liveness will fail to force re-registration", DriverName, ns.driver.nodeID)
		return false
	}

	return ns.kubeletHandshakeCurrent()
}

// kubeletHandshakeCurrent reports whether kubelet has called NodeGetInfo since
// it last started. The CSINode entry survives a kubelet restart but kubelet's
// in-memory plugin registry does not, so without a re-handshake mounts fail
// even though CSINode looks healthy. Fails open if the kubelet process cannot
// be located in /proc.
func (ns *NodeServer) kubeletHandshakeCurrent() bool {
	kubeletStart, ok := kubeletProcessStart()
	if !ok {
		klog.V(4).Info("registration check: kubelet process not found in /proc (failing open)")
		return true
	}
	if time.Since(kubeletStart) < kubeletRehandshakeGrace {
		return true
	}
	if nanos := ns.lastNodeGetInfo.Load(); nanos > 0 && time.Unix(0, nanos).After(kubeletStart) {
		return true
	}
	klog.Warningf("registration check: kubelet started at %s but has not re-registered driver %s "+
		"(no NodeGetInfo handshake since) — registrar liveness will fail to force re-registration",
		kubeletStart.Format(time.RFC3339), DriverName)
	return false
}

// kubeletPort is the kubelet's API port; its listener identifies the kubelet
// process regardless of distribution (plain kubelet, k3s, RKE2, microk8s...).
const kubeletPort = 10250

// kubeletProcessStart returns the start time of the kubelet process on this
// host, found via /proc (the node pod runs with hostPID+hostNetwork): the
// process listening on the kubelet port, falling back to known process names.
// If several name candidates match, the most recent start wins.
func kubeletProcessStart() (time.Time, bool) {
	bootTime, ok := hostBootTime()
	if !ok {
		return time.Time{}, false
	}

	if pid, ok := pidListeningOn(kubeletPort); ok {
		if ticks, ok := processStartTicks(pid); ok {
			return startTimeFromTicks(bootTime, ticks), true
		}
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return time.Time{}, false
	}
	var newest time.Time
	for _, e := range entries {
		pid := e.Name()
		if pid[0] < '0' || pid[0] > '9' {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + pid + "/cmdline")
		if err != nil {
			continue
		}
		if !isKubeletCmdline(strings.Split(string(cmdline), "\x00")) {
			continue
		}
		ticks, ok := processStartTicks(pid)
		if !ok {
			continue
		}
		if start := startTimeFromTicks(bootTime, ticks); start.After(newest) {
			newest = start
		}
	}
	return newest, !newest.IsZero()
}

// startTimeFromTicks converts a starttime in USER_HZ ticks since boot
// (USER_HZ is 100 on Linux) to wall-clock time.
func startTimeFromTicks(bootTime time.Time, ticks int64) time.Time {
	return bootTime.Add(time.Duration(ticks) * (time.Second / 100))
}

// pidListeningOn returns the pid of a process holding a TCP listening socket
// on the given port, resolved via /proc/net/tcp* socket inodes.
func pidListeningOn(port int) (string, bool) {
	inodes := map[string]bool{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		collectListeningInodes(f, port, inodes)
	}
	if len(inodes) == 0 {
		return "", false
	}

	procs, err := os.ReadDir("/proc")
	if err != nil {
		return "", false
	}
	for _, p := range procs {
		pid := p.Name()
		if pid[0] < '0' || pid[0] > '9' {
			continue
		}
		fds, err := os.ReadDir("/proc/" + pid + "/fd")
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink("/proc/" + pid + "/fd/" + fd.Name())
			if err == nil && inodes[link] {
				return pid, true
			}
		}
	}
	return "", false
}

// collectListeningInodes adds the "socket:[inode]" links of LISTEN sockets on
// the given port from a /proc/net/tcp-format file.
func collectListeningInodes(file string, port int, inodes map[string]bool) {
	data, err := os.ReadFile(file)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n")[1:] {
		fields := strings.Fields(line)
		// local_address=1, st=3 (0A is LISTEN), inode=9
		if len(fields) < 10 || fields[3] != "0A" {
			continue
		}
		i := strings.LastIndexByte(fields[1], ':')
		if i < 0 {
			continue
		}
		p, err := strconv.ParseUint(fields[1][i+1:], 16, 32)
		if err == nil && int(p) == port {
			inodes["socket:["+fields[9]+"]"] = true
		}
	}
}

// isKubeletCmdline reports whether a /proc cmdline belongs to a kubelet: the
// binary itself, or a k3s/RKE2/microk8s process embedding it.
func isKubeletCmdline(args []string) bool {
	if len(args) == 0 || args[0] == "" {
		return false
	}
	switch filepath.Base(args[0]) {
	case "kubelet", "kubelite":
		return true
	case "k3s", "k3s-server", "k3s-agent", "rke2", "rke2-server", "rke2-agent":
		return len(args) > 1 && (args[1] == "server" || args[1] == "agent")
	}
	return false
}

// processStartTicks returns field 22 (starttime) of /proc/<pid>/stat: the
// process start time in clock ticks since boot.
func processStartTicks(pid string) (int64, bool) {
	data, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return 0, false
	}
	// comm (field 2) may contain spaces; fields resume after its closing paren.
	rest := string(data)
	i := strings.LastIndexByte(rest, ')')
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(rest[i+1:])
	if len(fields) < 20 {
		return 0, false
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, false
	}
	return ticks, true
}

// hostBootTime returns the host boot time from /proc/stat's btime line.
func hostBootTime() (time.Time, bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, found := strings.CutPrefix(line, "btime "); found {
			secs, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err != nil {
				return time.Time{}, false
			}
			return time.Unix(secs, 0), true
		}
	}
	return time.Time{}, false
}
