//go:build fuseintegration

// Real-FUSE tests of the session layout; need root, /dev/fuse and fusermount3.
// Run with: make test-fuse
package rclone

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func requireFUSE(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("needs /dev/fuse")
	}
	if _, err := exec.LookPath("fusermount3"); err != nil {
		t.Skip("needs fusermount3")
	}
}

func mkdirIn(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// srcWith returns a local directory holding a marker file, the backend of one mount.
func srcWith(t *testing.T, marker string) string {
	t.Helper()
	dir := mkdirIn(t, t.TempDir(), "src")
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte(marker), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// mountFS mounts fs at mountPoint the way Mount does, returning its device.
func mountFS(t *testing.T, fs, mountPoint string) string {
	t.Helper()
	params := map[string]interface{}{
		"fs":         fs,
		"mountPoint": mountPoint,
		"mountOpt":   map[string]interface{}{"AllowNonEmpty": true, "AllowOther": true},
		"vfsOpt":     map[string]interface{}{"CachePollInterval": int64(time.Minute) + mountGeneration.Add(1)},
	}
	if _, err := RPCWithTimeout("mount/mount", params, rpcMountTimeout); err != nil {
		t.Fatalf("mount %s at %s: %v", fs, mountPoint, err)
	}
	t.Cleanup(func() {
		_, _ = RPCWithTimeout("mount/unmount", map[string]interface{}{"mountPoint": mountPoint}, rpcUnmountTimeout)
		_ = unmountBounded(mountPoint, syscall.MNT_DETACH)
	})
	var dev string
	if !waitFor(10*time.Second, func() bool {
		d, mounted, err := mountedDevice(mountPoint, 2*time.Second)
		dev = d
		return err == nil && mounted
	}) {
		t.Fatalf("%s never became a mountpoint", mountPoint)
	}
	return dev
}

func bind(t *testing.T, source, target string) {
	t.Helper()
	if err := bindBounded(source, target); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unmountBounded(target, syscall.MNT_DETACH) })
}

func detach(t *testing.T, path string) {
	t.Helper()
	if err := unmountBounded(path, syscall.MNT_DETACH); err != nil {
		t.Fatal(err)
	}
}

func waitFor(d time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return false
}

func servesDevice(path, dev string) bool {
	d, mounted, err := mountedDevice(path, 2*time.Second)
	return err == nil && mounted && d == dev
}

func marker(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "marker"))
	if err != nil {
		t.Fatalf("reading through %s: %v", dir, err)
	}
	return string(b)
}

// holdFor asserts path keeps serving dev for d: a teardown fires asynchronously.
func holdFor(t *testing.T, d time.Duration, path, dev string) {
	t.Helper()
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if !servesDevice(path, dev) {
			t.Fatalf("%s lost the successor's mount (%s)", path, dev)
		}
	}
}

// Control: with every generation at the staging path, killing the last consumer
// of the old one makes its teardown unmount the new one. If this stops failing
// the old way, the tests below no longer exercise the finalizer.
func TestFUSEOldLayoutPredecessorTeardownRipsSuccessor(t *testing.T) {
	requireFUSE(t)
	root := t.TempDir()
	staging, consumer := mkdirIn(t, root, "globalmount"), mkdirIn(t, root, "consumer")

	mountFS(t, ":local:"+srcWith(t, "gen1"), staging)
	bind(t, staging, consumer)
	detach(t, staging) // the globalmount vanishes; the consumer still holds gen1
	mountFS(t, ":local:"+srcWith(t, "gen2"), staging)
	detach(t, consumer) // consumer killed: gen1's last reference goes

	if !waitFor(10*time.Second, func() bool {
		_, mounted, _ := mountedDevice(staging, 2*time.Second)
		return !mounted
	}) {
		t.Fatal("gen1's teardown did not unmount gen2 at the shared path")
	}
}

// The incident's sequence under the session layout: the successor survives.
func TestFUSESessionLayoutPredecessorTeardownSparesSuccessor(t *testing.T) {
	requireFUSE(t)
	withSessionBase(t)
	root := t.TempDir()
	staging, consumer := mkdirIn(t, root, "globalmount"), mkdirIn(t, root, "consumer")

	s1, err := newSessionDir("pvc-t", "pvc-t.g1")
	if err != nil {
		t.Fatal(err)
	}
	mountFS(t, ":local:"+srcWith(t, "gen1"), s1)
	bind(t, s1, staging)
	bind(t, staging, consumer)
	detach(t, staging)

	s2, err := newSessionDir("pvc-t", "pvc-t.g2")
	if err != nil {
		t.Fatal(err)
	}
	dev2 := mountFS(t, ":local:"+srcWith(t, "gen2"), s2)
	bind(t, s2, staging)

	// Session 1 loses its own path and then its last consumer, as when a
	// consumer is restarted onto the repaired mount.
	detach(t, s1)
	detach(t, consumer)

	holdFor(t, 3*time.Second, staging, dev2)
	if got := marker(t, staging); got != "gen2" {
		t.Fatalf("staging serves %q, want gen2", got)
	}
	if live, ok := liveSessions("pvc-t"); !ok || !slices.Contains(live, s2) {
		t.Fatalf("session 2's record is gone (live=%v ok=%v); its teardown can no longer run VFS.Shutdown", live, ok)
	}
}

// A teardown that does find a mount at its path unmounts it — its own session
// only, never the staging path bound from a successor.
func TestFUSESessionTeardownUnmountsOnlyItsOwnPath(t *testing.T) {
	requireFUSE(t)
	withSessionBase(t)
	root := t.TempDir()
	staging := mkdirIn(t, root, "globalmount")

	s1, err := newSessionDir("pvc-t", "pvc-t.g1")
	if err != nil {
		t.Fatal(err)
	}
	dev1 := mountFS(t, ":local:"+srcWith(t, "gen1"), s1)
	s2, err := newSessionDir("pvc-t", "pvc-t.g2")
	if err != nil {
		t.Fatal(err)
	}
	dev2 := mountFS(t, ":local:"+srcWith(t, "gen2"), s2)
	bind(t, s2, staging)

	abortFUSE(t, dev1) // session 1's serve loop exits and its teardown runs

	if !waitFor(10*time.Second, func() bool {
		_, mounted, _ := mountedDevice(s1, 2*time.Second)
		return !mounted
	}) {
		t.Fatal("session 1's teardown never ran")
	}
	holdFor(t, 2*time.Second, staging, dev2)
	if live, _ := liveSessions("pvc-t"); !slices.Equal(live, []string{s2}) {
		t.Fatalf("live sessions = %v, want only %s", live, s2)
	}
}

// abortFUSE aborts the connection behind device dev (major:minor).
func abortFUSE(t *testing.T, dev string) {
	t.Helper()
	const conns = "/sys/fs/fuse/connections"
	if entries, _ := os.ReadDir(conns); len(entries) == 0 {
		if err := syscall.Mount("fusectl", conns, "fusectl", 0, ""); err != nil {
			t.Skipf("fusectl unavailable: %v", err)
		}
		t.Cleanup(func() { _ = syscall.Unmount(conns, syscall.MNT_DETACH) })
	}
	_, minor, _ := strings.Cut(dev, ":")
	if err := os.WriteFile(filepath.Join(conns, minor, "abort"), []byte("1"), 0200); err != nil {
		t.Fatalf("abort connection %s: %v", minor, err)
	}
}

// A staging path that lost its bind gets the same live session back, so the
// consumers already on it are left alone.
func TestFUSEReexposeLiveSession(t *testing.T) {
	requireFUSE(t)
	withSessionBase(t)
	root := t.TempDir()
	staging, consumer := mkdirIn(t, root, "globalmount"), mkdirIn(t, root, "consumer")

	const volumeID, name = "pvc-r", "pvc-r.g1"
	if _, err := RPC("config/create", map[string]interface{}{
		"name": name, "type": "local", "parameters": map[string]interface{}{},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { DeleteNamedConfigs(name) })

	s1, err := newSessionDir(volumeID, name)
	if err != nil {
		t.Fatal(err)
	}
	dev := mountFS(t, name+":"+srcWith(t, "gen1"), s1)
	mm := &MountManager{volumeID: volumeID, mountPoint: staging, sessionPath: s1, cryptConfigName: name, mounted: true}
	if err := mm.expose(dev, time.Now().Add(mountReadyTimeout)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mm.unbindStaging)
	bind(t, staging, consumer)

	detach(t, staging)
	if !mm.SessionServing() {
		t.Fatal("a session that lost only its staging bind must still count as serving")
	}
	if err := mm.Reexpose(); err != nil {
		t.Fatal(err)
	}
	if !servesDevice(staging, dev) || !servesDevice(consumer, dev) {
		t.Fatal("staging and consumer must both be on the original session")
	}
	if got := marker(t, staging); got != "gen1" {
		t.Fatalf("staging serves %q, want gen1", got)
	}
}

// A consumer bound to the bare staging directory on a shared mount joins its
// peer group: unwinding that consumer then unmounts the staging path too
// (seen on si-algonomia). The session survives it and can be re-exposed.
func TestFUSECrossLinkedConsumerUnwindIsRecoverable(t *testing.T) {
	requireFUSE(t)
	withSessionBase(t)
	root := t.TempDir()
	if err := syscall.Mount("tmpfs", root, "tmpfs", 0, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Unmount(root, syscall.MNT_DETACH) })
	if err := syscall.Mount("", root, "", syscall.MS_SHARED, ""); err != nil {
		t.Fatal(err)
	}
	staging, consumer := mkdirIn(t, root, "globalmount"), mkdirIn(t, root, "consumer")
	bind(t, staging, consumer) // the bad bind: bare directory, root's peer group

	const volumeID, name = "pvc-x", "pvc-x.g1"
	if _, err := RPC("config/create", map[string]interface{}{
		"name": name, "type": "local", "parameters": map[string]interface{}{},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { DeleteNamedConfigs(name) })
	s1, err := newSessionDir(volumeID, name)
	if err != nil {
		t.Fatal(err)
	}
	dev := mountFS(t, name+":"+srcWith(t, "gen1"), s1)
	mm := &MountManager{volumeID: volumeID, mountPoint: staging, sessionPath: s1, cryptConfigName: name, mounted: true}
	if err := bindBounded(s1, staging); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mm.unbindStaging)

	if !servesDevice(consumer, dev) {
		t.Fatal("control: the staging bind did not propagate onto the cross-linked consumer")
	}
	detach(t, consumer) // what re-pointing the consumer does first
	if _, mounted, _ := mountedDevice(staging, 2*time.Second); mounted {
		t.Fatal("control: unwinding the consumer did not take the staging bind with it")
	}

	if !mm.SessionServing() {
		t.Fatal("the session must survive losing its staging bind")
	}
	if err := mm.Reexpose(); err != nil {
		t.Fatal(err)
	}
	if !servesDevice(staging, dev) {
		t.Fatal("staging path not re-exposed")
	}
}
