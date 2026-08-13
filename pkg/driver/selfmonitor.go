package driver

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/lukscryptwalker-csi/pkg/asynclog"
	"github.com/lukscryptwalker-csi/pkg/rclone"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
)

// The driver dies abruptly with no panic, no signal and no kernel OOM record,
// leaving nothing to explain it. Resource growth is the remaining measurable
// hypothesis, so record it continuously: these lines land in the container log,
// which survives on disk (/var/log/pods/.../N.log) even when the sandbox is
// removed and the container record is destroyed.
const (
	selfMonitorInterval = 60 * time.Second

	// Warn well before Go's 10000-thread abort, and before a plausible
	// memory ceiling, so the trend is visible while there is still time.
	threadWarnThreshold    = 2000
	goroutineWarnThreshold = 10000
	rssWarnKB              = 1024 * 1024 // 1 GiB
)

// runSelfMonitor logs process resource usage every minute. Cheap, and the only
// continuous record of what the process was doing before it disappeared.
func (ns *NodeServer) runSelfMonitor() {
	ticker := time.NewTicker(selfMonitorInterval)
	defer ticker.Stop()

	var peakThreads, peakRSS int
	for range ticker.C {
		ns.reportLogStall()
		ns.verifyCacheStillEncrypted()

		goroutines := runtime.NumGoroutine()
		threads := procStatusField("Threads")
		rssKB := procStatusField("VmRSS")
		fds := countOpenFDs()

		if threads > peakThreads {
			peakThreads = threads
		}
		if rssKB > peakRSS {
			peakRSS = rssKB
		}

		klog.Infof("selfmon: goroutines=%d threads=%d(peak %d) rss=%dMi(peak %dMi) fds=%d",
			goroutines, threads, peakThreads, rssKB/1024, peakRSS/1024, fds)

		if threads >= threadWarnThreshold || goroutines >= goroutineWarnThreshold || rssKB >= rssWarnKB {
			// Straight to stderr and bounded: a queued line would not survive
			// the death this is warning about.
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			asynclog.WriteBounded(os.Stderr,
				fmt.Sprintf("RESOURCE-ALERT: goroutines=%d threads=%d rss=%dMi fds=%d; goroutine dump follows\n%s\n",
					goroutines, threads, rssKB/1024, fds, buf[:n]), 10*time.Second)
		}
	}
}

// verifyCacheStillEncrypted re-checks that the VFS cache is still its LUKS
// volume: startup proves it once, but a later stray umount would send cached
// contents to the node's disk in the clear. Reported, not fatal — exiting
// would tear down every mount on the node, which is the operator's call.
func (ns *NodeServer) verifyCacheStillEncrypted() {
	err := rclone.VerifyCacheEncrypted()
	wasBroken := ns.cacheUnencrypted.Swap(err != nil)

	if err == nil {
		if wasBroken && ns.recorder != nil {
			ns.recorder.Eventf(ns.nodeRef(), corev1.EventTypeNormal, "VFSCacheEncryptionRestored",
				"The VFS cache is mounted on its LUKS volume again")
		}
		return
	}

	klog.Errorf("PLAINTEXT RISK: %v", err)
	if wasBroken || ns.recorder == nil {
		return // one event per occurrence
	}
	ns.recorder.Eventf(ns.nodeRef(), corev1.EventTypeWarning, "VFSCacheNotEncrypted",
		"The CSI driver's VFS cache is not on its encrypted volume: %v. Cached contents of S3-backed volumes "+
			"are being written to this node's disk in plaintext. Restart the driver pod on this node.", err)
}

// logStallEventThreshold is how long output must be blocked to be worth an event.
const logStallEventThreshold = 60 * time.Second

// reportLogStall raises a Node event while the process cannot emit log lines:
// a blocked log pipe silences everything once the buffer fills, so the driver
// reads as frozen while it is serving fine. Events go via the API, not the pipe.
func (ns *NodeServer) reportLogStall() {
	stalled := asynclog.DefaultStalledFor()
	wasStalled := ns.logStalled.Swap(stalled >= logStallEventThreshold)

	if stalled < logStallEventThreshold {
		if wasStalled && ns.recorder != nil {
			ns.recorder.Eventf(ns.nodeRef(), corev1.EventTypeNormal, "DriverLogOutputRecovered",
				"Driver log output is flowing again; the gap in the container log was blocked output, not inactivity")
		}
		return
	}
	if wasStalled || ns.recorder == nil {
		return // one event per stall, not one per minute
	}
	ns.recorder.Eventf(ns.nodeRef(), corev1.EventTypeWarning, "DriverLogOutputStalled",
		"The CSI driver has been unable to write a log line for %s (blocked container log pipe). The driver is "+
			"still running and its mounts are still served — an empty container log is NOT evidence of a freeze.",
		stalled.Round(time.Second))
}

// nodeRef is the object reference for events about this node.
func (ns *NodeServer) nodeRef() *corev1.ObjectReference {
	return &corev1.ObjectReference{Kind: "Node", Name: ns.driver.nodeID, UID: types.UID(ns.driver.nodeID)}
}

// procStatusField reads a numeric field (kB or count) from /proc/self/status.
func procStatusField(name string) int {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, name+":") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return -1
		}
		v, err := strconv.Atoi(fields[1])
		if err != nil {
			return -1
		}
		return v
	}
	return -1
}

// countOpenFDs returns the number of open file descriptors, or -1.
func countOpenFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(entries)
}
