package driver

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

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
			// Straight to stderr: if this is the run-up to the process dying,
			// a queued line will not survive it.
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			fmt.Fprintf(os.Stderr, "RESOURCE-ALERT: goroutines=%d threads=%d rss=%dMi fds=%d; goroutine dump follows\n%s\n",
				goroutines, threads, rssKB/1024, fds, buf[:n])
		}
	}
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
