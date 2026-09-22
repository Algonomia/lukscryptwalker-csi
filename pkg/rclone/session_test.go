package rclone

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

func withSessionBase(t *testing.T) string {
	t.Helper()
	orig := sessionBase
	sessionBase = filepath.Join(t.TempDir(), "sessions")
	t.Cleanup(func() { sessionBase = orig })
	return sessionBase
}

// A session path is what rclone's teardown unmounts, so no two sessions may
// ever share one.
func TestNewSessionDirNeverReusesAPath(t *testing.T) {
	base := withSessionBase(t)
	seen := map[string]bool{}
	for range 200 {
		dir, err := newSessionDir("pvc-a", "pvc-a.g3")
		if err != nil {
			t.Fatal(err)
		}
		if seen[dir] {
			t.Fatalf("session path %s handed out twice", dir)
		}
		seen[dir] = true
		if filepath.Dir(dir) != filepath.Join(base, "pvc-a") {
			t.Fatalf("session %s is not under its volume's directory", dir)
		}
	}
}

// Sessions expose decrypted volumes without mode checks; only root may reach them.
func TestSessionTreeIsRootOnly(t *testing.T) {
	base := withSessionBase(t)
	if err := os.MkdirAll(base, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := newSessionDir("pvc-a", "pvc-a"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0700 {
		t.Errorf("session base mode = %o, want 700", perm)
	}
}

// A volume must never pick up, or shut down, another volume's sessions.
func TestSessionsOfMatchesOnlyThatVolume(t *testing.T) {
	base := withSessionBase(t)
	mounts := []string{
		filepath.Join(base, "pvc-ab", "pvc-ab@1"),
		filepath.Join(base, "pvc-a", "pvc-a.g2@3"),
		"/var/lib/kubelet/plugins/kubernetes.io/csi/x/globalmount",
		filepath.Join(base, "pvc-a", "pvc-a@2"),
		filepath.Join(base, "pvc-a"),
	}
	got := sessionsOf("pvc-a", mounts)
	want := []string{filepath.Join(base, "pvc-a", "pvc-a.g2@3"), filepath.Join(base, "pvc-a", "pvc-a@2")}
	if !slices.Equal(got, want) {
		t.Errorf("sessionsOf = %v, want %v", got, want)
	}
}

// mountUsable compares a stat device against mountinfo's major:minor field.
func TestDevStringMatchesMountinfo(t *testing.T) {
	for _, c := range []struct {
		major, minor uint32
		want         string
	}{{0, 328, "0:328"}, {259, 3, "259:3"}, {0, 1048575, "0:1048575"}} {
		if got := devString(unix.Mkdev(c.major, c.minor)); got != c.want {
			t.Errorf("devString(%d:%d) = %q, want %q", c.major, c.minor, got, c.want)
		}
	}
}
