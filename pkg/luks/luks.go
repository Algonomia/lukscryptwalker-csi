package luks

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/klog/v2"
)

const (
	CryptsetupCmd = "cryptsetup"
)

type LUKSManager struct{}

func NewLUKSManager() *LUKSManager {
	return &LUKSManager{}
}

// FormatAndOpenLUKS creates a LUKS encrypted volume and opens it
func (lm *LUKSManager) FormatAndOpenLUKS(devicePath, mapperName, passphrase string) error {
	klog.V(4).Infof("Formatting LUKS device: %s", devicePath)
	
	// Check if already LUKS formatted
	if lm.IsLUKSDevice(devicePath) {
		klog.V(4).Infof("Device %s is already LUKS formatted", devicePath)
		return lm.OpenLUKS(devicePath, mapperName, passphrase)
	}

	// Format the device with LUKS
	cmd := exec.Command(CryptsetupCmd, "luksFormat", "--batch-mode", devicePath)
	cmd.Stdin = strings.NewReader(passphrase)
	cmd.Stderr = os.Stderr
	
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to format LUKS device %s: %v", devicePath, err)
	}

	// Open the LUKS device
	return lm.OpenLUKS(devicePath, mapperName, passphrase)
}

// OpenLUKS opens an existing LUKS device
func (lm *LUKSManager) OpenLUKS(devicePath, mapperName, passphrase string) error {
	klog.V(4).Infof("Opening LUKS device: %s as %s", devicePath, mapperName)
	
	// Check if already opened
	if lm.IsLUKSOpened(mapperName) {
		klog.V(4).Infof("LUKS device %s is already opened as %s", devicePath, mapperName)
		return nil
	}

	cmd := exec.Command(CryptsetupCmd, "luksOpen", devicePath, mapperName)
	cmd.Stdin = strings.NewReader(passphrase)
	cmd.Stderr = os.Stderr
	
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to open LUKS device %s: %v", devicePath, err)
	}

	return nil
}

// CloseLUKS closes a LUKS device
func (lm *LUKSManager) CloseLUKS(mapperName string) error {
	if !lm.IsLUKSOpened(mapperName) {
		klog.V(4).Infof("LUKS device %s is not opened", mapperName)
		return nil
	}

	klog.V(4).Infof("Closing LUKS device: %s", mapperName)
	
	cmd := exec.Command(CryptsetupCmd, "luksClose", mapperName)
	cmd.Stderr = os.Stderr
	
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to close LUKS device %s: %v", mapperName, err)
	}

	return nil
}

// IsLUKSDevice checks if a device is LUKS formatted
func (lm *LUKSManager) IsLUKSDevice(devicePath string) bool {
	cmd := exec.Command(CryptsetupCmd, "isLuks", devicePath)
	return cmd.Run() == nil
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
func (lm *LUKSManager) ResizeLUKS(mapperName string) error {
	if !lm.IsLUKSOpened(mapperName) {
		return fmt.Errorf("LUKS device %s is not opened", mapperName)
	}

	klog.V(4).Infof("Resizing LUKS device: %s", mapperName)

	cmd := exec.Command(CryptsetupCmd, "resize", mapperName)
	cmd.Stderr = os.Stderr
	
	if err := cmd.Run(); err != nil {
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