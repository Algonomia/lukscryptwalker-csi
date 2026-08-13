package rclone

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestReadDirRawListsEntriesWithTypes(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("a.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	entries, err := ReadDirRaw(dir, 100)
	if err != nil {
		t.Fatalf("ReadDirRaw: %v", err)
	}

	got := map[string]DirEntryRaw{}
	var names []string
	for _, e := range entries {
		got[e.Name] = e
		names = append(names, e.Name)
	}
	sort.Strings(names)

	if len(entries) != 4 {
		t.Fatalf("got %d entries %v, want 4 (. and .. must be skipped)", len(entries), names)
	}
	if !got["subdir"].IsDir {
		t.Error("subdir not reported as a directory")
	}
	if !got["a.txt"].IsRegular || got["a.txt"].IsDir {
		t.Error("a.txt not reported as a regular file")
	}
	if got["link"].IsRegular || got["link"].IsDir {
		t.Error("a symlink should be neither regular nor a directory")
	}
}

func TestReadDirRawRespectsMax(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"a", "b", "c", "d", "e"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := ReadDirRaw(dir, 2)
	if err != nil {
		t.Fatalf("ReadDirRaw: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("got %d entries, want the requested maximum of 2", len(entries))
	}
}

func TestReadDirRawErrors(t *testing.T) {
	if _, err := ReadDirRaw(filepath.Join(t.TempDir(), "absent"), 10); err == nil {
		t.Error("listing a missing directory reported success")
	}
	// A regular file is not a directory: O_DIRECTORY must reject it.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDirRaw(f, 10); err == nil {
		t.Error("listing a regular file reported success")
	}
}

func TestOpenProbeRaw(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := OpenProbeRaw(f); err != nil {
		t.Errorf("probing a readable file failed: %v", err)
	}
	if err := OpenProbeRaw(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("probing a missing file reported success")
	}
}

// The probe must not leak descriptors: the checker runs it every 30s per mount,
// and a leak would exhaust the process's fd table over days.
func TestRawProbesCloseTheirDescriptors(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	before := openFDCount(t)
	for range 50 {
		if _, err := ReadDirRaw(dir, 10); err != nil {
			t.Fatal(err)
		}
		if err := OpenProbeRaw(f); err != nil {
			t.Fatal(err)
		}
	}
	if after := openFDCount(t); after > before+2 {
		t.Errorf("descriptor count grew from %d to %d over 50 probes", before, after)
	}
}

func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd on this platform")
	}
	return len(entries)
}

// Guard the invariant the whole file exists for: the probe helpers must use raw
// syscalls. A regular os.File would be registered with Go's netpoller, and on a
// wedged FUSE mount that hangs the entire runtime inside epoll_ctl.
func TestProbeHelpersDoNotUseOsFile(t *testing.T) {
	src, err := os.ReadFile("fusesafe.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue // the explanatory comment names os.Open on purpose
		}
		for _, banned := range []string{"os.Open(", "os.ReadDir(", "os.NewFile("} {
			if strings.Contains(line, banned) {
				t.Errorf("%s appears in fusesafe.go: descriptors here must never reach the netpoller\n  %s",
					banned, strings.TrimSpace(line))
			}
		}
	}
	// And the raw path must actually be syscall-based.
	if !strings.Contains(string(src), "syscall.Open(") || !strings.Contains(string(src), "syscall.ReadDirent(") {
		t.Error("fusesafe.go no longer uses raw syscalls")
	}
}
