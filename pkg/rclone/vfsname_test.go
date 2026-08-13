package rclone

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A leaked VFS forces a volume onto a new name; the name must round-trip so a
// driver restart resumes on the cache dir the volume last wrote to.
func TestVFSNameGenerationRoundTrip(t *testing.T) {
	const vol = "pvc-8cd82035-1111-2222-3333-444455556666"

	for _, gen := range []int{0, 1, 7, 16} {
		vfsName, s3Name, cryptName := vfsNamesFor(vol, gen)
		if got := generationOfVFSName(vol, vfsName); got != gen {
			t.Errorf("generation %d: round-tripped to %d via %q", gen, got, vfsName)
		}
		if got := volumeIDOfVFSName(vfsName); got != vol {
			t.Errorf("generation %d: volumeIDOfVFSName(%q) = %q, want %q", gen, vfsName, got, vol)
		}
		if cryptName != vfsName {
			t.Errorf("generation %d: crypt config %q must equal the vfs name %q (the cache dir is derived from it)",
				gen, cryptName, vfsName)
		}
		if s3Name != vfsName+"-s3" {
			t.Errorf("generation %d: s3 config = %q, want %q", gen, s3Name, vfsName+"-s3")
		}
	}

	// Generation 0 keeps the bare volumeID so caches written by earlier
	// versions keep resuming after an upgrade.
	if vfsName, _, _ := vfsNamesFor(vol, 0); vfsName != vol {
		t.Errorf("generation 0 = %q, want the bare volume id %q", vfsName, vol)
	}
	// Distinct generations must not collide — that is the whole point.
	a, _, _ := vfsNamesFor(vol, 1)
	b, _, _ := vfsNamesFor(vol, 2)
	if a == b {
		t.Errorf("generations 1 and 2 both named %q", a)
	}
}

func TestGenerationOfVFSNameRejectsGarbage(t *testing.T) {
	const vol = "pvc-abc"
	for _, name := range []string{"", "pvc-abc", "pvc-other.g2", "pvc-abc.gx", "pvc-abc.g", "pvc-abc-g3"} {
		if got := generationOfVFSName(vol, name); got != 0 {
			t.Errorf("generationOfVFSName(%q, %q) = %d, want 0", vol, name, got)
		}
	}
}

func TestVolumeIDOfVFSNameLeavesPlainNames(t *testing.T) {
	for _, name := range []string{"pvc-abc", "pvc-abc.gx", ".g1"} {
		if got := volumeIDOfVFSName(name); got != name {
			t.Errorf("volumeIDOfVFSName(%q) = %q, want it unchanged", name, got)
		}
	}
}

// writeCacheItem creates a cache data file plus its metadata, dirty or not.
func writeCacheItem(t *testing.T, base, vfsName, item string, dirty bool) {
	t.Helper()
	dataDir := filepath.Join(base, "vfs", vfsName)
	metaDir := filepath.Join(base, "vfsMeta", vfsName)
	for _, d := range []string{dataDir, metaDir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dataDir, item), []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	meta, err := json.Marshal(cacheItemInfo{Size: 7, Dirty: dirty})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, item), meta, 0600); err != nil {
		t.Fatal(err)
	}
}

func cacheDirExists(base, vfsName string) bool {
	_, err := os.Stat(filepath.Join(base, "vfs", vfsName))
	return err == nil
}

// Unmapped cache dirs may be reclaimed only when nothing in them is still
// waiting to be uploaded — those bytes are the only copy of the writes.
func TestSweepUnmappedVFSCacheDirs(t *testing.T) {
	base := t.TempDir()

	writeCacheItem(t, base, "pvc-live", "f", false)    // live volume, current name
	writeCacheItem(t, base, "pvc-live.g1", "f", false) // abandoned generation, clean
	writeCacheItem(t, base, "pvc-live.g2", "f", true)  // abandoned generation, DIRTY
	writeCacheItem(t, base, "pvc-gone", "f", false)    // deleted volume, clean
	writeCacheItem(t, base, "pvc-gone.g1", "f", true)  // deleted volume, DIRTY
	writeCacheItem(t, base, "pvc-mapped", "f", false)  // still referenced by the map

	nameMap := map[string]string{"pvc-mapped": "pvc-mapped"}
	isActive := func(volumeID string) bool { return volumeID == "pvc-live" || volumeID == "pvc-mapped" }

	sweepUnmappedVFSCacheDirsAt(base, nameMap, isActive)

	keep := []string{"pvc-live", "pvc-live.g2", "pvc-gone.g1", "pvc-mapped"}
	drop := []string{"pvc-live.g1", "pvc-gone"}
	for _, name := range keep {
		if !cacheDirExists(base, name) {
			t.Errorf("%s was reclaimed but must be kept", name)
		}
	}
	for _, name := range drop {
		if cacheDirExists(base, name) {
			t.Errorf("%s should have been reclaimed", name)
		}
	}
}

func TestHasDirtyCacheItems(t *testing.T) {
	base := t.TempDir()
	writeCacheItem(t, base, "clean", "a", false)
	writeCacheItem(t, base, "clean", "b", false)
	writeCacheItem(t, base, "mixed", "a", false)
	writeCacheItem(t, base, "mixed", "b", true)

	if hasDirtyCacheItemsAt(base, "clean") {
		t.Error("all-clean cache reported as dirty")
	}
	if !hasDirtyCacheItemsAt(base, "mixed") {
		t.Error("cache with one unuploaded item reported as clean")
	}
	// A cache with no metadata at all has nothing pending.
	if hasDirtyCacheItemsAt(base, "absent") {
		t.Error("missing cache reported as dirty")
	}
}
