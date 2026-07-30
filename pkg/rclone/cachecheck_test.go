package rclone

import (
	"os"
	"path/filepath"
	"testing"
)

func writeItem(t *testing.T, base, vfsName, rel, meta string, dataSize int) (metaPath, dataPath string) {
	t.Helper()
	metaPath = filepath.Join(base, "vfsMeta", vfsName, rel)
	dataPath = filepath.Join(base, "vfs", vfsName, rel)
	for _, p := range []string{metaPath, dataPath} {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(metaPath, []byte(meta), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataPath, make([]byte, dataSize), 0644); err != nil {
		t.Fatal(err)
	}
	return metaPath, dataPath
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestValidateVFSCache(t *testing.T) {
	const vol = "pvc-test"

	t.Run("healthy item kept", func(t *testing.T) {
		base := t.TempDir()
		m, d := writeItem(t, base, vol, "db/data.mdb",
			`{"Size":100,"Dirty":false,"Rs":[{"Pos":0,"Size":100}]}`, 100)
		validateVFSCacheAt(base, vol)
		if !exists(m) || !exists(d) {
			t.Error("healthy item was removed")
		}
	})

	t.Run("range past EOF, clean: removed", func(t *testing.T) {
		base := t.TempDir()
		m, d := writeItem(t, base, vol, "db/data.mdb",
			`{"Size":1000,"Dirty":false,"Rs":[{"Pos":0,"Size":1000}]}`, 100)
		validateVFSCacheAt(base, vol)
		if exists(m) || exists(d) {
			t.Error("corrupt clean item was not removed")
		}
	})

	t.Run("range past EOF, dirty: quarantined", func(t *testing.T) {
		base := t.TempDir()
		m, d := writeItem(t, base, vol, "db/data.mdb",
			`{"Size":1000,"Dirty":true,"Rs":[{"Pos":0,"Size":1000}]}`, 100)
		validateVFSCacheAt(base, vol)
		if exists(m) || exists(d) {
			t.Error("corrupt dirty item left in place")
		}
		q, err := filepath.Glob(filepath.Join(base, "quarantine", vol, "db", "data.mdb.*"))
		if err != nil || len(q) != 2 {
			t.Errorf("expected 2 quarantined files, got %v", q)
		}
	})

	t.Run("garbage metadata: removed", func(t *testing.T) {
		base := t.TempDir()
		m, d := writeItem(t, base, vol, "x/y", `not json at all`, 10)
		validateVFSCacheAt(base, vol)
		if exists(m) || exists(d) {
			t.Error("garbage-meta item was not removed")
		}
	})

	t.Run("no cache dir: no-op", func(t *testing.T) {
		validateVFSCacheAt(t.TempDir(), vol)
	})
}
