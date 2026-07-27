package driver

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandBackingFileAlreadyAtSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backing.img")
	if err := os.WriteFile(path, make([]byte, 4096), 0600); err != nil {
		t.Fatal(err)
	}

	// Same size and smaller must both no-op without touching the file.
	for _, size := range []int64{4096, 1024} {
		if err := ExpandBackingFile(path, size); err != nil {
			t.Fatalf("ExpandBackingFile(%d) on 4096-byte file: %v", size, err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() != 4096 {
			t.Fatalf("file size changed to %d, want 4096", fi.Size())
		}
	}
}

func TestExpandBackingFileGrows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backing.img")
	if err := os.WriteFile(path, make([]byte, 4096), 0600); err != nil {
		t.Fatal(err)
	}

	if err := ExpandBackingFile(path, 65536); err != nil {
		t.Fatalf("ExpandBackingFile grow: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 65536 {
		t.Fatalf("file size = %d after grow, want 65536", fi.Size())
	}
}
