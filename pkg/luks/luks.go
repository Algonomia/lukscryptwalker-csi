package luks

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog"
)

// CryptsetupCmd is the cryptsetup binary; a var so tests can substitute a stub.
var CryptsetupCmd = "cryptsetup"

const (
	// Without a bound, `cryptsetup close` can wait forever on a device-mapper
	// udev cookie, pinning the loop device and the backing file's blocks.
	cryptsetupTimeout = 2 * time.Minute
)

type LUKSManager struct{}

func NewLUKSManager() *LUKSManager {
	return &LUKSManager{}
}

// runCryptsetup runs cryptsetup with a hard timeout, retrying once through
// udevcomplete_all if it looks stuck on a device-mapper cookie. udev sync
// itself is disabled via DM_DISABLE_UDEV in runBounded.
func runCryptsetup(stdin string, args ...string) error {
	err := runBounded(cryptsetupTimeout, stdin, CryptsetupCmd, args...)
	if err == nil || !isTimeout(err) {
		return err
	}

	klog.Warningf("cryptsetup %v did not return within %s (stuck on a device-mapper udev cookie?); "+
		"releasing pending cookies and retrying once", args, cryptsetupTimeout)
	if cerr := runBounded(30*time.Second, "", "dmsetup", "udevcomplete_all", "-y"); cerr != nil {
		klog.Warningf("dmsetup udevcomplete_all failed: %v", cerr)
	}
	return runBounded(cryptsetupTimeout, stdin, CryptsetupCmd, args...)
}

// errTimeout marks a command killed for exceeding its budget.
type errTimeout struct {
	name    string
	timeout time.Duration
}

func (e *errTimeout) Error() string {
	return fmt.Sprintf("%s timed out after %s", e.name, e.timeout)
}

func isTimeout(err error) bool {
	_, ok := err.(*errTimeout)
	return ok
}

// runBounded runs a command with a hard timeout, never waiting on a child
// wedged in uninterruptible sleep. DM_DISABLE_UDEV because our uevents are
// never completed by the host's udevd, so a cookie wait would hang forever —
// it is an env var read by libdevmapper; cryptsetup has no such flag.
func runBounded(timeout time.Duration, stdin, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "DM_DISABLE_UDEV=1")
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out strings.Builder
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil && out.Len() > 0 {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(out.String()))
		}
		return err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return &errTimeout{name: name, timeout: timeout}
	}
}

// FormatAndOpenLUKS creates a LUKS encrypted volume and opens it
func (lm *LUKSManager) FormatAndOpenLUKS(devicePath, mapperName, passphrase string) error {
	klog.Infof("Formatting LUKS device: %s", devicePath)

	// Check if already LUKS formatted
	if lm.IsLUKSDevice(devicePath) {
		klog.Infof("Device %s is already LUKS formatted", devicePath)
		return lm.OpenLUKS(devicePath, mapperName, passphrase)
	}

	// Format the device with LUKS
	if err := runCryptsetup(passphrase, "luksFormat", "--batch-mode", devicePath); err != nil {
		return fmt.Errorf("failed to format LUKS device %s: %v", devicePath, err)
	}

	// Open the LUKS device
	return lm.OpenLUKS(devicePath, mapperName, passphrase)
}

// OpenLUKS opens an existing LUKS device
func (lm *LUKSManager) OpenLUKS(devicePath, mapperName, passphrase string) error {
	klog.Infof("Opening LUKS device: %s as %s", devicePath, mapperName)

	// Check if already opened
	if lm.IsLUKSOpened(mapperName) {
		klog.Infof("LUKS device %s is already opened as %s", devicePath, mapperName)
		return nil
	}

	if err := runCryptsetup(passphrase, "luksOpen", devicePath, mapperName); err != nil {
		return fmt.Errorf("failed to open LUKS device %s: %v", devicePath, err)
	}

	return nil
}

// CloseLUKS closes a LUKS device
func (lm *LUKSManager) CloseLUKS(mapperName string) error {
	if !lm.IsLUKSOpened(mapperName) {
		klog.Infof("LUKS device %s is not opened", mapperName)
		return nil
	}

	klog.Infof("Closing LUKS device: %s", mapperName)

	if err := runCryptsetup("", "luksClose", mapperName); err != nil {
		return fmt.Errorf("failed to close LUKS device %s: %v", mapperName, err)
	}

	return nil
}

// IsLUKSDevice checks if a device is LUKS formatted
func (lm *LUKSManager) IsLUKSDevice(devicePath string) bool {
	return runBounded(30*time.Second, "", CryptsetupCmd, "isLuks", devicePath) == nil
}

// IsLUKSOpened checks if a LUKS device is opened
func (lm *LUKSManager) IsLUKSOpened(mapperName string) bool {
	mapperPath := filepath.Join("/dev/mapper", mapperName)
	_, err := os.Stat(mapperPath)
	return err == nil
}

// GetMappedDevicePath returns the path to the mapped device
func (lm *LUKSManager) GetMappedDevicePath(mapperName string) string {
	return filepath.Join("/dev/mapper", mapperName)
}

// GenerateMapperName generates a unique mapper name for a volume
func (lm *LUKSManager) GenerateMapperName(volumeID string) string {
	return fmt.Sprintf("luks-%s", volumeID)
}

// ResizeLUKS resizes a LUKS device to fill the underlying block device
func (lm *LUKSManager) ResizeLUKS(mapperName, passphrase string) error {
	if !lm.IsLUKSOpened(mapperName) {
		return fmt.Errorf("LUKS device %s is not opened", mapperName)
	}

	klog.Infof("Resizing LUKS device: %s", mapperName)

	if err := runCryptsetup(passphrase, "resize", mapperName); err != nil {
		return fmt.Errorf("failed to resize LUKS device %s: %v", mapperName, err)
	}

	return nil
}

// GetLUKSDeviceSize returns the size of the underlying LUKS device in bytes
func (lm *LUKSManager) GetLUKSDeviceSize(devicePath string) (int64, error) {
	cmd := exec.Command("blockdev", "--getsize64", devicePath)
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("failed to get device size for %s: %v", devicePath, err)
	}

	sizeStr := strings.TrimSpace(string(output))
	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse device size %s: %v", sizeStr, err)
	}

	return size, nil
}
