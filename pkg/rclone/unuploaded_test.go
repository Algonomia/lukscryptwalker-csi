package rclone

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Once the mount manager is gone the cache dir is the only evidence left, and a
// mount that hit a leaked VFS name wrote it under a .gN suffix — so the check
// has to span every generation of the volume, not just the current name.
func TestHasUnuploadedDataSpansGenerations(t *testing.T) {
	base := t.TempDir()

	writeCacheItem(t, base, "pvc-clean", "f", false)
	writeCacheItem(t, base, "pvc-clean.g1", "f", false)
	writeCacheItem(t, base, "pvc-stranded", "f", false)
	writeCacheItem(t, base, "pvc-stranded.g2", "f", true) // abandoned generation, DIRTY

	if hasUnuploadedDataAt(base, "pvc-clean") {
		t.Error("fully uploaded volume reported as holding unuploaded data")
	}
	if !hasUnuploadedDataAt(base, "pvc-stranded") {
		t.Error("unuploaded writes in an abandoned generation were missed — unstaging would report the volume " +
			"detached while this node held the only copy")
	}
	if hasUnuploadedDataAt(base, "pvc-never-mounted") {
		t.Error("volume with no cache at all reported as holding unuploaded data")
	}
	if hasUnuploadedDataAt(base, "") {
		t.Error("empty volume id reported as holding unuploaded data")
	}
}

// A prefix match would make pvc-abc answer for pvc-abcdef, which would pin a
// healthy volume on the node forever.
func TestHasUnuploadedDataDoesNotMatchOnPrefix(t *testing.T) {
	base := t.TempDir()
	writeCacheItem(t, base, "pvc-abcdef", "f", true)

	if hasUnuploadedDataAt(base, "pvc-abc") {
		t.Error("pvc-abc matched the cache of pvc-abcdef")
	}
	if !hasUnuploadedDataAt(base, "pvc-abcdef") {
		t.Error("pvc-abcdef did not match its own cache")
	}
}

// An unreadable metadata tree is not proof the writes reached S3.
func TestHasUnuploadedDataErrsTowardsDirty(t *testing.T) {
	base := t.TempDir()
	writeCacheItem(t, base, "pvc-unreadable", "f", false)

	metaDir := filepath.Join(base, "vfsMeta", "pvc-unreadable")
	if err := os.Chmod(metaDir, 0000); err != nil {
		t.Skipf("cannot drop directory permissions here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(metaDir, 0700) })
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not make the tree unreadable")
	}

	if !hasUnuploadedDataAt(base, "pvc-unreadable") {
		t.Error("unreadable cache metadata was reported as fully uploaded")
	}
}

// The sweep that recovers volumes kubelet has forgotten needs the volume ids,
// deduplicated across generations.
func TestVolumesWithUnuploadedData(t *testing.T) {
	base := t.TempDir()

	writeCacheItem(t, base, "pvc-a", "f", true)
	writeCacheItem(t, base, "pvc-a.g1", "f", true) // same volume, second generation
	writeCacheItem(t, base, "pvc-b", "f", false)
	writeCacheItem(t, base, "pvc-c.g3", "f", true)

	got := volumesWithUnuploadedDataAt(base)
	want := []string{"pvc-a", "pvc-c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("volumesWithUnuploadedDataAt = %v, want %v", got, want)
	}

	if got := volumesWithUnuploadedDataAt(t.TempDir()); got != nil {
		t.Errorf("empty cache produced %v, want nil", got)
	}
}

// Corrupt metadata used to read as "clean", which let the sweeper reclaim a
// cache dir holding the only copy of unuploaded writes.
func TestCacheItemDirtyErrsTowardsDirty(t *testing.T) {
	dir := t.TempDir()

	truncated := filepath.Join(dir, "truncated")
	if err := os.WriteFile(truncated, []byte(`{"Size":7,"Dir`), 0600); err != nil {
		t.Fatal(err)
	}
	if !isCacheItemDirty(truncated) {
		t.Error("unparseable metadata reported as uploaded")
	}

	if !isCacheItemDirty(filepath.Join(dir, "does-not-exist")) {
		t.Error("unreadable metadata reported as uploaded")
	}

	clean := filepath.Join(dir, "clean")
	if err := os.WriteFile(clean, []byte(`{"Size":7,"Dirty":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	if isCacheItemDirty(clean) {
		t.Error("uploaded item reported as dirty")
	}
}
