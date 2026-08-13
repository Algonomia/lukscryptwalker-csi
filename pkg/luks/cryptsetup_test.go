package luks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubCryptsetup installs a fake cryptsetup that records its argv and the
// DM_DISABLE_UDEV it was given, then exits with the requested code.
func stubCryptsetup(t *testing.T, exitCode int) (argvFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv")
	script := filepath.Join(dir, "cryptsetup")

	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + argvFile + "\n" +
		"printf 'DM_DISABLE_UDEV=%s\\n' \"$DM_DISABLE_UDEV\" >> " + argvFile + "\n" +
		"exit " + string(rune('0'+exitCode)) + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	orig := CryptsetupCmd
	CryptsetupCmd = script
	t.Cleanup(func() { CryptsetupCmd = orig })
	return argvFile
}

// The driver must pass cryptsetup exactly the arguments the caller asked for.
// Injecting an extra global flag broke every invocation on a build that does
// not have it (cryptsetup 2.8.6 has no --disable-udev), and since the VFS cache
// setup is fatal on error, that took the whole driver down at startup.
func TestRunCryptsetupPassesArgsVerbatim(t *testing.T) {
	argvFile := stubCryptsetup(t, 0)

	if err := runCryptsetup("", "luksOpen", "/dev/loop6", "luks-vfs-cache"); err != nil {
		t.Fatalf("runCryptsetup: %v", err)
	}

	recorded, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(recorded)), "\n")
	args := lines[:len(lines)-1] // last line is the env probe

	want := []string{"luksOpen", "/dev/loop6", "luks-vfs-cache"}
	if len(args) != len(want) {
		t.Fatalf("cryptsetup got %d args %q, want exactly %q — no flag may be injected", len(args), args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("arg %d = %q, want %q", i, args[i], want[i])
		}
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--disable-udev") {
			t.Error("--disable-udev was injected; cryptsetup has no such option and rejects it")
		}
	}
}

// Udev cookie waits are suppressed through libdevmapper's environment variable,
// which works on every build — that is the actual BUG 4 mitigation.
func TestRunCryptsetupSetsDMDisableUdev(t *testing.T) {
	argvFile := stubCryptsetup(t, 0)

	if err := runCryptsetup("", "luksClose", "luks-vfs-cache"); err != nil {
		t.Fatalf("runCryptsetup: %v", err)
	}

	recorded, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(recorded), "DM_DISABLE_UDEV=1") {
		t.Errorf("cryptsetup ran without DM_DISABLE_UDEV=1; a close can then hang forever on a udev cookie.\ngot: %s", recorded)
	}
}

// A non-zero exit must surface as an error, with cryptsetup's own stderr folded
// in — "exit status 1" alone is what made the usage-error failure unreadable.
func TestRunCryptsetupReportsFailure(t *testing.T) {
	stubCryptsetup(t, 1)

	err := runCryptsetup("", "luksOpen", "/dev/loop6", "luks-vfs-cache")
	if err == nil {
		t.Fatal("a failing cryptsetup reported success")
	}
}
