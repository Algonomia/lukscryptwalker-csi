package rclone

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDirCacheWarmInterval(t *testing.T) {
	tests := []struct {
		name         string
		dirCacheTime string
		want         time.Duration
	}{
		{"default 1h renews at 48m", "1h", 48 * time.Minute},
		{"empty falls back to the 1h default", "", 48 * time.Minute},
		{"long cache renews proportionally", "24h", 24 * time.Hour * 8 / 10},
		{"short cache is floored, not a listing loop", "30s", time.Minute},
		{"caching off disables the warmer", "0", 0},
		{"unparseable disables the warmer", "later", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mm := &MountManager{vfsConfig: &VFSCacheConfig{DirCacheTime: tt.dirCacheTime}}
			if got := mm.dirCacheWarmInterval(); got != tt.want {
				t.Errorf("dirCacheWarmInterval() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The warm interval must land strictly inside DirCacheTime, or the listing
// expires before renewal and a consumer pays for it.
func TestDirCacheWarmIntervalBeatsExpiry(t *testing.T) {
	for _, d := range []string{"2m", "10m", "1h", "6h", "24h"} {
		mm := &MountManager{vfsConfig: &VFSCacheConfig{DirCacheTime: d}}
		expiry, err := parseDurationToNs(d)
		if err != nil {
			t.Fatalf("parseDurationToNs(%q): %v", d, err)
		}
		if warm := mm.dirCacheWarmInterval(); warm >= time.Duration(expiry) {
			t.Errorf("DirCacheTime %s: renews every %v, at or past expiry %v", d, warm, time.Duration(expiry))
		}
	}
}

func TestResolveUnder(t *testing.T) {
	t.Run("unresolved root passes paths through", func(t *testing.T) {
		got := resolveUnder("/cache", "/cache", "/cache/db/data.mdb")
		if want := "/cache/db/data.mdb"; got != want {
			t.Errorf("resolveUnder() = %q, want %q", got, want)
		}
	})

	t.Run("symlinked root is rewritten to the kernel's namespace", func(t *testing.T) {
		got := resolveUnder("/cache", "/mnt/real", "/cache/db/data.mdb")
		if want := "/mnt/real/db/data.mdb"; got != want {
			t.Errorf("resolveUnder() = %q, want %q", got, want)
		}
	})
}

func TestOpenFilesUnder(t *testing.T) {
	root := t.TempDir()

	held := filepath.Join(root, "held.bin")
	if err := os.WriteFile(held, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	idle := filepath.Join(root, "idle.bin")
	if err := os.WriteFile(idle, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	fd, err := os.Open(held)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()

	open, realRoot, ok := openFilesUnder(root)
	if !ok {
		t.Fatal("openFilesUnder reported the scan as untrustworthy")
	}

	if _, found := open[resolveUnder(root, realRoot, held)]; !found {
		t.Error("file with an open descriptor was not reported as open (it would be evicted from under rclone)")
	}
	if _, found := open[resolveUnder(root, realRoot, idle)]; found {
		t.Error("file with no open descriptor was reported as open (it would never be evicted)")
	}
}
