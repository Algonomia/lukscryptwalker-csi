package rclone

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"k8s.io/klog"
)

// SessionMountBase holds one private mountpoint per rclone session; the staging
// path is only ever a bind of one. rclone's teardown unmounts by path, so a
// path no other session uses is one no other session's teardown can reach.
const SessionMountBase = "/var/lib/lukscrypt-cache/sessions"

// sessionBase is SessionMountBase, swappable in tests.
var sessionBase = SessionMountBase

// bindTimeout bounds mount/umount syscalls, which queue on the kernel mount lock.
const bindTimeout = 30 * time.Second

// newSessionDir creates a mountpoint no session, in this process or an earlier
// one, has used.
func newSessionDir(volumeID, vfsName string) (string, error) {
	parent := filepath.Join(sessionBase, volumeID)
	if err := mkdirAllBounded(parent, 0700, bindTimeout); err != nil {
		return "", err
	}
	// Sessions serve decrypted data with no mode checks (allow_other), so the
	// tree must be root-only, like the kubelet tree the globalmount lives in.
	if err := os.Chmod(sessionBase, 0700); err != nil { // #nosec G302 -- a directory: root needs its search bit
		return "", err
	}
	for {
		dir := filepath.Join(parent, fmt.Sprintf("%s@%d", vfsName, time.Now().UnixNano()))
		err := os.Mkdir(dir, 0700)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
}

// sessionsOf picks one volume's sessions out of rclone's mount list.
func sessionsOf(volumeID string, mountPoints []string) []string {
	prefix := filepath.Join(sessionBase, volumeID) + "/"
	var out []string
	for _, mp := range mountPoints {
		if strings.HasPrefix(mp, prefix) {
			out = append(out, mp)
		}
	}
	sort.Strings(out)
	return out
}

// liveSessions returns the volume's sessions rclone still holds a mount record
// for, and whether rclone could answer.
func liveSessions(volumeID string) ([]string, bool) {
	result, err := RPCWithTimeout("mount/listmounts", map[string]interface{}{}, rpcListTimeout)
	if err != nil || result == nil || result.Output == nil {
		klog.Warningf("Could not list rclone mounts: %v", err)
		return nil, false
	}
	entries, _ := result.Output["mountPoints"].([]interface{})
	var mountPoints []string
	for _, e := range entries {
		if m, ok := e.(map[string]interface{}); ok {
			if mp, ok := m["MountPoint"].(string); ok {
				mountPoints = append(mountPoints, mp)
			}
		}
	}
	return sessionsOf(volumeID, mountPoints), true
}

// ShutdownSessions ends every live session of the volume, plus any of known
// that rclone's mount list missed, through mount/unmount so VFS.Shutdown runs.
// Returns how many rclone still held.
func ShutdownSessions(volumeID string, known ...string) int {
	sessions, _ := liveSessions(volumeID)
	for _, k := range known {
		if k != "" && !slices.Contains(sessions, k) {
			sessions = append(sessions, k)
		}
	}
	ended := 0
	for _, s := range sessions {
		if unmountSession(s) {
			ended++
		}
	}
	return ended
}

// unmountSession ends one session and removes its mountpoint. Reports whether
// rclone still had a record of it.
func unmountSession(path string) bool {
	found := true
	if _, err := RPCWithTimeout("mount/unmount", map[string]interface{}{"mountPoint": path}, rpcUnmountTimeout); err != nil {
		if strings.Contains(err.Error(), "mount not found") {
			found = false
		} else {
			// VFS.Shutdown ran before the kernel unmount failed; finish the detach.
			klog.Warningf("Session %s: mount/unmount failed (%v); detaching lazily", path, err)
		}
		_ = unmountBounded(path, syscall.MNT_DETACH)
	} else {
		klog.Infof("Session %s shut down", path)
	}
	removeSessionDir(path)
	return found
}

// SweepDeadSessions detaches the volume's session mountpoints rclone holds no
// record of: leftovers of a previous driver process, whose daemon is gone.
// Callers must keep the volume's mounts from starting meanwhile.
func SweepDeadSessions(volumeID string) {
	live, ok := liveSessions(volumeID)
	if !ok {
		return
	}
	isLive := make(map[string]bool, len(live))
	for _, s := range live {
		isLive[s] = true
	}
	parent := filepath.Join(sessionBase, volumeID)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	for _, e := range entries {
		path := filepath.Join(parent, e.Name())
		if isLive[path] {
			continue
		}
		if err := unmountBounded(path, syscall.MNT_DETACH); err == nil {
			klog.Infof("Detached dead session %s", path)
		}
		removeSessionDir(path)
	}
	_ = os.Remove(parent) // only if empty
}

// SessionVolumes lists the volumes that have session mountpoints on this node.
func SessionVolumes() []string {
	entries, err := os.ReadDir(sessionBase)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// removeSessionDir removes an emptied session mountpoint. Never recursive: if a
// session is somehow still attached there, its contents are the volume in S3.
func removeSessionDir(path string) {
	done := make(chan error, 1)
	go func() { done <- os.Remove(path) }()
	select {
	case err := <-done:
		if err != nil && !os.IsNotExist(err) {
			klog.Warningf("Could not remove session mountpoint %s: %v", path, err)
		}
	case <-time.After(bindTimeout):
		klog.Warningf("Removing session mountpoint %s blocked for %s", path, bindTimeout)
	}
}

// bindBounded binds source onto target in our mount namespace, from which it
// propagates to the host like any mount under the kubelet tree.
func bindBounded(source, target string) error {
	return syscallBounded(fmt.Sprintf("bind %s to %s", source, target), func() error {
		return syscall.Mount(source, target, "", syscall.MS_BIND, "")
	})
}

// unmountBounded unmounts the topmost mount at path.
func unmountBounded(path string, flags int) error {
	return syscallBounded("unmount "+path, func() error { return syscall.Unmount(path, flags) })
}

func syscallBounded(what string, call func() error) error {
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	case <-time.After(bindTimeout):
		return fmt.Errorf("%s blocked for %s (the kernel mount lock is held)", what, bindTimeout)
	}
}

// mountedDevice returns the device serving path, as mountinfo spells it, when
// path is a mountpoint (its device differs from its parent's).
func mountedDevice(path string, timeout time.Duration) (string, bool, error) {
	type result struct {
		self, parent syscall.Stat_t
		err          error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		if r.err = syscall.Stat(path, &r.self); r.err == nil {
			r.err = syscall.Stat(filepath.Dir(path), &r.parent)
		}
		done <- r
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return "", false, r.err
		}
		return devString(uint64(r.self.Dev)), r.self.Dev != r.parent.Dev, nil
	case <-time.After(timeout):
		return "", false, fmt.Errorf("stat %s blocked for %s (wedged FUSE)", path, timeout)
	}
}

// devString formats a device number the way mountinfo does (major:minor).
func devString(dev uint64) string {
	return fmt.Sprintf("%d:%d", unix.Major(dev), unix.Minor(dev))
}
