package driver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// sparseFile creates a file of apparent size n with no allocated blocks, skipping
// the test on filesystems that don't support holes.
func sparseFile(t *testing.T, path string, n int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(n); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if holes, _ := unreservedBytes(path); holes != n {
		t.Skipf("filesystem does not keep %s sparse (unreserved %d of %d)", path, holes, n)
	}
}

func hostAvail(t *testing.T, dir string) int64 {
	t.Helper()
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

func TestUnreservedBytes(t *testing.T) {
	dir := t.TempDir()
	written := filepath.Join(dir, "written.img")
	if err := os.WriteFile(written, make([]byte, 1<<16), 0600); err != nil {
		t.Fatal(err)
	}
	if n, err := unreservedBytes(written); err != nil || n != 0 {
		t.Fatalf("written file: unreserved = %d, %v; want 0", n, err)
	}

	sparse := filepath.Join(dir, "sparse.img")
	sparseFile(t, sparse, 1<<20)
	if n, _ := unreservedBytes(sparse); n != 1<<20 {
		t.Fatalf("sparse file: unreserved = %d, want %d", n, 1<<20)
	}
}

// The incident shape: another volume's holes already claim the free space, so a
// plain free-space check passes while the host is in fact overcommitted.
func TestEnsureBackingSpaceCountsSparseSiblings(t *testing.T) {
	root := t.TempDir()
	avail := hostAvail(t, root)
	sparseFile(t, filepath.Join(root, "pvc-a", "luks-pvc-a.img"), avail)

	newFile := filepath.Join(root, "pvc-b", "luks-pvc-b.img")
	if err := os.MkdirAll(filepath.Dir(newFile), 0755); err != nil {
		t.Fatal(err)
	}
	const need = 1 << 20
	if err := ensureFreeSpace(filepath.Dir(newFile), need); err != nil {
		t.Skipf("host too full for this test: %v", err)
	}
	err := ensureBackingSpace(newFile, need)
	if err == nil || !strings.Contains(err.Error(), "already promised") {
		t.Fatalf("ensureBackingSpace = %v, want refusal citing promised space", err)
	}
}

func TestReserveBackingFileFillsHoles(t *testing.T) {
	if _, err := exec.LookPath("fallocate"); err != nil {
		t.Skip("fallocate not installed")
	}
	path := filepath.Join(t.TempDir(), "luks-pvc.img")
	sparseFile(t, path, 1<<20)

	left, err := reserveBackingFile(path)
	if err != nil {
		t.Skipf("fallocate unsupported here: %v", err)
	}
	if left != 0 {
		t.Fatalf("unreserved after reserve = %d, want 0", left)
	}
	if fi, _ := os.Stat(path); fi.Size() != 1<<20 {
		t.Fatalf("size changed to %d", fi.Size())
	}
}

func TestBackingSpaceHint(t *testing.T) {
	dir := t.TempDir()
	written := filepath.Join(dir, "written.img")
	if err := os.WriteFile(written, make([]byte, 4096), 0600); err != nil {
		t.Fatal(err)
	}
	if h := backingSpaceHint(written); h != "" {
		t.Fatalf("fully allocated file got hint %q", h)
	}

	// Holes larger than the free space: the incident.
	big := filepath.Join(dir, "big.img")
	sparseFile(t, big, hostAvail(t, dir)+1<<30)
	if h := backingSpaceHint(big); !strings.Contains(h, "fallocate") {
		t.Fatalf("overcommitted file: hint = %q, want fallocate advice", h)
	}

	// Holes the host can still cover are not the cause of a mount failure.
	small := filepath.Join(dir, "small.img")
	sparseFile(t, small, 1<<20)
	if h := backingSpaceHint(small); h != "" {
		t.Fatalf("coverable holes got hint %q", h)
	}
}

func TestClassifyBlkidProbe(t *testing.T) {
	for _, tc := range []struct {
		code      int
		formatted bool
		wantErr   bool
	}{
		{0, true, false},
		{2, false, false},
		{8, true, false},
		{4, false, true},
		{-1, false, true}, // blkid missing or killed: never "blank"
	} {
		formatted, err := classifyBlkidProbe(tc.code)
		if formatted != tc.formatted || (err != nil) != tc.wantErr {
			t.Errorf("code %d: got (%v, %v), want (%v, err=%v)", tc.code, formatted, err, tc.formatted, tc.wantErr)
		}
	}
}

func TestHasFilesystemSignature(t *testing.T) {
	if _, err := exec.LookPath("blkid"); err != nil {
		t.Skip("blkid not installed")
	}
	blank := filepath.Join(t.TempDir(), "blank.img")
	if err := os.WriteFile(blank, make([]byte, 4<<20), 0600); err != nil {
		t.Fatal(err)
	}
	if formatted, err := hasFilesystemSignature(blank); err != nil || formatted {
		t.Fatalf("blank image: (%v, %v), want (false, nil)", formatted, err)
	}

	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not installed")
	}
	if out, err := exec.Command("mkfs.ext4", "-q", "-F", blank).CombinedOutput(); err != nil {
		t.Skipf("mkfs.ext4: %v: %s", err, out)
	}
	if formatted, err := hasFilesystemSignature(blank); err != nil || !formatted {
		t.Fatalf("ext4 image: (%v, %v), want (true, nil)", formatted, err)
	}
}

func TestReadDeviceHead(t *testing.T) {
	short := filepath.Join(t.TempDir(), "short.img")
	if err := os.WriteFile(short, make([]byte, 512), 0600); err != nil {
		t.Fatal(err)
	}
	if err := readDeviceHead(short, 1<<20); err != nil {
		t.Fatalf("short device: %v", err)
	}
	if err := readDeviceHead(filepath.Join(t.TempDir(), "missing"), 1<<20); err == nil {
		t.Fatal("missing device: want error")
	}
}
