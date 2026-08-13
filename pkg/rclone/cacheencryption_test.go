package rclone

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An ordinary directory is NOT the encrypted volume. This is the case that
// matters: if the LUKS mount is missing, the cache path is a plain directory on
// the node's disk and every cached file body lands there in the clear.
func TestVerifyCacheEncryptedRejectsPlainDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rclone")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	err := verifyCacheEncryptedAt(dir)
	if err == nil {
		t.Fatal("a plain directory was accepted as the encrypted cache — plaintext would be written to the node disk")
	}
	if !strings.Contains(err.Error(), "plaintext") {
		t.Errorf("error should say what is at stake, got: %v", err)
	}
}

// A missing cache dir must be an error too, not a silent pass.
func TestVerifyCacheEncryptedRejectsMissingDir(t *testing.T) {
	if err := verifyCacheEncryptedAt(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("a missing cache dir was accepted")
	}
}

// The check must key on the device, not on the path looking plausible.
func TestVerifyCacheEncryptedComparesDevices(t *testing.T) {
	// /proc is a distinct filesystem from /, so a path under it has a different
	// st_dev than its parent — the same signal a mounted LUKS volume gives.
	if _, err := os.Stat("/proc/self"); err != nil {
		t.Skip("no /proc on this platform")
	}
	if err := verifyCacheEncryptedAt("/proc"); err != nil {
		t.Errorf("a path on its own filesystem was rejected: %v", err)
	}
}
