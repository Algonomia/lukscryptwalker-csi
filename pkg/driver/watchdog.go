package driver

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
)

// The host watchdog heals exactly one state nothing in-cluster can: the
// shim-zombie — the container runtime reports our driver container Running
// while no driver process exists, so kubelet never restarts it and storage
// stays down. Its ONLY cure is stopping our own dead sandbox so kubelet
// recreates it. It never touches the runtime, the control plane, or the node:
// v1 of this script held runtime-restart and reboot powers and amplified a
// driver outage into a cluster outage.
const watchdogScript = `#!/bin/sh
# lukscrypt-watchdog v2 — installed by lukscryptwalker-csi.
# Acts ONLY on the shim-zombie signature (runtime says Running, no process).
# Cure: stop+remove OUR pod sandbox. Nothing else, ever.
RUNSTATE=/run/lukscrypt-watchdog     # tmpfs: a reboot resets all counters
LOG=/var/lib/lukscrypt-watchdog/actions.log
mkdir -p "$RUNSTATE" /var/lib/lukscrypt-watchdog

crictl_cmd() {
  for c in crictl /var/lib/rancher/rke2/bin/crictl /var/lib/rancher/k3s/data/current/bin/crictl; do
    { command -v "$c" >/dev/null 2>&1 || [ -x "$c" ]; } || continue
    for ep in unix:///run/containerd/containerd.sock unix:///run/k3s/containerd/containerd.sock; do
      if "$c" --runtime-endpoint "$ep" version >/dev/null 2>&1; then
        echo "$c --runtime-endpoint $ep"
        return 0
      fi
    done
  done
  return 1
}

# Healthy or legitimately absent: not our business.
if pgrep -f 'lukscryptwalker-csi --endpoint=unix:///csi/csi.sock' >/dev/null 2>&1; then
  rm -f "$RUNSTATE/zombie-count"
  exit 0
fi
CR=$(crictl_cmd) || exit 0
RUNNING=$($CR ps --name lukscryptwalker-csi -q 2>/dev/null)
if [ -z "$RUNNING" ]; then
  # No process AND no Running claim: normal crashloop/startup/uninstall —
  # kubelet's problem, not ours.
  rm -f "$RUNSTATE/zombie-count"
  exit 0
fi

# Signature present: runtime claims Running, no process. Require it to hold
# for 10 consecutive minutes before acting.
N=$(cat "$RUNSTATE/zombie-count" 2>/dev/null || echo 0)
N=$((N + 1))
echo "$N" > "$RUNSTATE/zombie-count"
logger -t lukscrypt-watchdog "shim-zombie signature: runtime reports driver Running, no process ($N/3)"
# 3 minutes, not 10: the original window existed to let kubelet act first,
# but in this state containerd never records the exit, so kubelet never acts
# at all — the extra 7 minutes were pure outage for every mount on the node.
[ "$N" -lt 3 ] && exit 0

# At most one sandbox kill per 15 minutes.
now=$(date +%s)
last=$(cat "$RUNSTATE/last-kill" 2>/dev/null || echo 0)
[ $((now - last)) -lt 900 ] && exit 0
echo "$now" > "$RUNSTATE/last-kill"
rm -f "$RUNSTATE/zombie-count"

POD=$($CR pods --name csi-node -q 2>/dev/null | head -1)
[ -z "$POD" ] && exit 0

# Scope the container lookup to THIS sandbox: the controller pods run
# containers with the same name on this node, and an unscoped lookup captured
# a controller's logs as "evidence" for a dead node driver.
CID=$($CR ps -a --pod "$POD" --name lukscryptwalker-csi -q 2>/dev/null | head -1)

# The dead driver left its FUSE mounts with no server. Unmounting those hangs
# forever, so containerd cannot tear the sandbox down — stopp/rmp fail
# silently and the zombie survives. Abort the connections first: their server
# is gone, so they can only ever serve EIO, and aborting lets teardown finish.
ABORTED=0
for conn in $(grep fuse.rclone /proc/self/mountinfo 2>/dev/null | awk '{print $3}' | cut -d: -f2 | sort -u); do
  [ -w "/sys/fs/fuse/connections/$conn/abort" ] || continue
  echo 1 > "/sys/fs/fuse/connections/$conn/abort" 2>/dev/null && ABORTED=$((ABORTED + 1))
done
[ "$ABORTED" -gt 0 ] && logger -t lukscrypt-watchdog "aborted $ABORTED orphaned rclone FUSE connection(s) of the dead driver"

# Removing the sandbox destroys the container's exit record and its final log
# lines — the very evidence needed to explain why the driver died. Capture it
# first, or every recovery erases the reason it was needed.
if [ -n "$CID" ]; then
  {
    echo "=== $(date -Is) driver container $CID state before sandbox removal ==="
    timeout 20 $CR inspect "$CID" 2>&1 | grep -iE '"(exitCode|reason|message|startedAt|finishedAt|oomKilled)"' | head -20
    echo "--- last log lines ---"
    timeout 20 $CR logs --tail 40 "$CID" 2>&1 | tail -40
  } >> "$LOG" 2>&1
  logger -t lukscrypt-watchdog "captured dead driver container $CID state to $LOG"
fi

logger -t lukscrypt-watchdog "removing zombie driver sandbox $POD so kubelet recreates it"
OUT=$( { timeout 60 $CR stopp "$POD"; timeout 60 $CR rmp -f "$POD"; } 2>&1 )
RC=$?
if [ "$RC" -eq 0 ]; then
  logger -t lukscrypt-watchdog "zombie driver sandbox $POD removed"
  echo "$(date -Is) removed zombie driver sandbox $POD after aborting $ABORTED orphaned FUSE connection(s)" >> "$LOG"
else
  # Never silent: a cure that cannot work must say so, or it looks like healing.
  logger -t lukscrypt-watchdog "FAILED to remove zombie sandbox $POD (rc=$RC): $OUT"
  echo "$(date -Is) FAILED to remove zombie driver sandbox $POD (rc=$RC): $OUT" >> "$LOG"
fi
exit 0
`

const watchdogService = `[Unit]
Description=lukscryptwalker-csi self-heal watchdog (container-scoped)

[Service]
Type=oneshot
ExecStart=/var/lib/lukscrypt-watchdog/watchdog.sh
TimeoutStartSec=180
`

const watchdogTimer = `[Unit]
Description=Run lukscryptwalker-csi self-heal watchdog every minute

[Timer]
OnBootSec=120
OnUnitActiveSec=60

[Install]
WantedBy=timers.target
`

// InstallHostWatchdog writes the v2 watchdog to the host and enables its
// timer, purging any v1 escalation state (counters, reboot stamps) on the
// way. Idempotent; failures are logged, not fatal.
func InstallHostWatchdog() {
	if os.Getenv("HOST_WATCHDOG") == "false" {
		klog.Info("Host watchdog disabled via HOST_WATCHDOG=false")
		_, _ = runHostSh("systemctl disable --now lukscrypt-watchdog.timer 2>/dev/null; true", "", 30*time.Second)
		return
	}
	steps := []struct {
		content string
		cmd     string
	}{
		// v1 kept escalation counters in /var/lib and could restart the
		// runtime or reboot; make sure none of that state survives.
		{"", "rm -f /var/lib/lukscrypt-watchdog/unhealthy-count /var/lib/lukscrypt-watchdog/last-reboot; true"},
		{watchdogScript, "mkdir -p /var/lib/lukscrypt-watchdog && cat > /var/lib/lukscrypt-watchdog/watchdog.sh && chmod 755 /var/lib/lukscrypt-watchdog/watchdog.sh"},
		{watchdogService, "cat > /etc/systemd/system/lukscrypt-watchdog.service"},
		{watchdogTimer, "cat > /etc/systemd/system/lukscrypt-watchdog.timer"},
		{"", "systemctl daemon-reload && systemctl enable --now lukscrypt-watchdog.timer"},
	}
	for _, s := range steps {
		if _, err := runHostSh(s.cmd, s.content, 30*time.Second); err != nil {
			klog.Errorf("Host watchdog install step failed (%s): %v", s.cmd, err)
			return
		}
	}
	klog.Info("Host self-heal watchdog v2 installed (container-scoped, every 60s)")
}

// reportWatchdogActions surfaces actions the watchdog took while no driver was
// alive to witness them — as logs and Node events — then clears the log.
func (ns *NodeServer) reportWatchdogActions() {
	// Archive before truncating: Node events expire in ~1h, so reporting must
	// not be the only copy — the captured cause of a death has to survive
	// until someone looks for it.
	const drainCmd = `L=/var/lib/lukscrypt-watchdog/actions.log
cat "$L" 2>/dev/null
cat "$L" >> /var/lib/lukscrypt-watchdog/actions-archive.log 2>/dev/null
: > "$L" 2>/dev/null
true`
	out, err := runHostSh(drainCmd, "", 15*time.Second)
	if err != nil || strings.TrimSpace(out) == "" {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		klog.Warningf("Host watchdog acted while the driver was down: %s", line)
		if ns.recorder != nil {
			nodeRef := &corev1.ObjectReference{Kind: "Node", Name: ns.driver.nodeID, UID: types.UID(ns.driver.nodeID)}
			ns.recorder.Eventf(nodeRef, corev1.EventTypeWarning, "HostWatchdogRecovery", "%s", line)
		}
	}
}

// runHostSh runs a shell command in the host mount namespace with optional
// stdin, returning stdout, bounded so a stalled host can never hang the caller.
func runHostSh(command, stdin string, timeout time.Duration) (string, error) {
	cmd := exec.Command("nsenter", "-t", "1", "-m", "sh", "-c", command)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out strings.Builder
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.String(), err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return out.String(), fmt.Errorf("timed out after %s", timeout)
	}
}
