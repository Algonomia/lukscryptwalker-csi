package rclone

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/lukscryptwalker-csi/pkg/luks"
	"github.com/rclone/rclone/fs/config"
	"k8s.io/klog"
)

var (
	// vfsCacheSize stores the configured VFS cache size in bytes
	vfsCacheSize int64 = 20 * 1024 * 1024 * 1024 // 20GB default
	// vfsNameMapMu protects reads/writes to the vfsName map file
	vfsNameMapMu sync.Mutex
)

const vfsNameMapFile = VFSCacheBasePath + "/.vfs-names.json"

const (
	// VFSCacheBasePath is the base path for the encrypted VFS cache
	// Mount directly at rclone's default cache location
	VFSCacheBasePath = "/root/.cache/rclone"
	// VFSCacheMapperName is the LUKS mapper name for VFS cache
	VFSCacheMapperName = "luks-vfs-cache"
	// VFSCacheBackingFile is the backing file for VFS cache LUKS volume
	// Stored inside the host-mounted vfs-cache-dir so it persists across pod restarts,
	// allowing rclone to resume syncing cached data that wasn't uploaded before a crash
	VFSCacheBackingFile = "/var/lib/lukscrypt-vfs-cache/vfs-cache.luks"
)

// vfsCacheNodeIDFile records the node ID the cache volume was formatted for.
// The passphrase embeds the node ID, so without this a node rename orphans the
// cache: cryptsetup reports only "No key available with this passphrase", the
// driver exits, and every volume on the node goes down over a lost cache.
// Overridable in tests.
var vfsCacheNodeIDFile = "/var/lib/lukscrypt-vfs-cache/.node-id"

// vfsCachePassphrase derives the cache passphrase, preferring the node ID the
// volume was actually formatted with so renames stay recoverable.
func vfsCachePassphrase(basePassphrase, nodeID string) string {
	if recorded, err := os.ReadFile(vfsCacheNodeIDFile); err == nil {
		if id := strings.TrimSpace(string(recorded)); id != "" && id != nodeID {
			klog.Warningf("VFS cache was formatted for node %q but this node is now %q (renamed?) — "+
				"using the recorded id so the existing cache still opens", id, nodeID)
			return fmt.Sprintf("%s-%s", basePassphrase, id)
		}
	}
	return fmt.Sprintf("%s-%s", basePassphrase, nodeID)
}

// recordVFSCacheNodeID stores the node ID used to format the cache volume.
func recordVFSCacheNodeID(nodeID string) {
	if err := os.WriteFile(vfsCacheNodeIDFile, []byte(nodeID+"\n"), 0600); err != nil {
		klog.Warningf("Could not record VFS cache node id (a node rename will orphan the cache): %v", err)
	}
}

// SetupVFSCache creates and mounts an encrypted LUKS volume for VFS cache
// Returns the mount path or empty string if setup fails
func SetupVFSCache(sizeStr string, basePassphrase string, nodeID string) (string, error) {
	passphrase := vfsCachePassphrase(basePassphrase, nodeID)
	// Mount directly at the rclone cache base path
	mountPath := VFSCacheBasePath

	// Parse size string to bytes (default to 20GB if parsing fails)
	cacheSize := int64(20 * 1024 * 1024 * 1024) // 20GB default
	if sizeStr != "" {
		if parsed, err := parseSizeToBytes(sizeStr); err == nil && parsed > 0 {
			cacheSize = parsed
		} else {
			klog.Warningf("Failed to parse VFS cache size '%s', using default 20GB: %v", sizeStr, err)
		}
	}

	// Store the cache size globally for later use
	vfsCacheSize = cacheSize

	klog.Infof("Setting up encrypted VFS cache: size=%d bytes, backing=%s, mount=%s",
		cacheSize, VFSCacheBackingFile, mountPath)

	// Check if already mounted
	if isVFSCacheMounted(mountPath) {
		// Check if resize is needed even though it's already mounted
		if info, err := os.Stat(VFSCacheBackingFile); err == nil && info.Size() < cacheSize {
			klog.Infof("VFS cache mounted at %s but backing file (%d bytes) is smaller than configured size (%d bytes), unmounting to resize",
				mountPath, info.Size(), cacheSize)
			cleanupStaleVFSCache(mountPath)
			// Fall through to re-setup with the new size
		} else {
			klog.Infof("VFS cache already mounted at %s", mountPath)
			if err := pinAndVerifyCacheDir(mountPath); err != nil {
				return "", err
			}
			return mountPath, nil
		}
	}

	// Clean up any stale state
	cleanupStaleVFSCache(mountPath)

	// Create directories
	if err := os.MkdirAll(filepath.Dir(VFSCacheBackingFile), 0700); err != nil {
		return "", fmt.Errorf("failed to create VFS cache directory: %w", err)
	}
	if err := os.MkdirAll(mountPath, 0700); err != nil {
		return "", fmt.Errorf("failed to create VFS cache mount directory: %w", err)
	}

	luksManager := luks.NewLUKSManager()

	// Create backing file if it doesn't exist
	if _, err := os.Stat(VFSCacheBackingFile); os.IsNotExist(err) {
		klog.Infof("Creating VFS cache backing file %s with size %d bytes", VFSCacheBackingFile, cacheSize)

		// Create sparse file
		f, err := os.Create(VFSCacheBackingFile)
		if err != nil {
			return "", fmt.Errorf("failed to create VFS cache backing file: %w", err)
		}
		if err := f.Truncate(cacheSize); err != nil {
			_ = f.Close()
			_ = os.Remove(VFSCacheBackingFile)
			return "", fmt.Errorf("failed to set VFS cache backing file size: %w", err)
		}
		_ = f.Close()

		// Setup loop device
		loopDevice, err := setupLoopDevice(VFSCacheBackingFile)
		if err != nil {
			_ = os.Remove(VFSCacheBackingFile)
			return "", fmt.Errorf("failed to setup loop device: %w", err)
		}

		// Format with LUKS
		if err := luksManager.FormatAndOpenLUKS(loopDevice, VFSCacheMapperName, passphrase); err != nil {
			_ = detachLoopDevice(loopDevice)
			_ = os.Remove(VFSCacheBackingFile)
			return "", fmt.Errorf("failed to format LUKS VFS cache: %w", err)
		}

		// Format the mapped device with ext4
		mappedDevice := luksManager.GetMappedDevicePath(VFSCacheMapperName)
		if err := formatExt4(mappedDevice); err != nil {
			_ = luksManager.CloseLUKS(VFSCacheMapperName)
			_ = detachLoopDevice(loopDevice)
			_ = os.Remove(VFSCacheBackingFile)
			return "", fmt.Errorf("failed to format VFS cache filesystem: %w", err)
		}

		// Remember which node ID this volume's passphrase was built from.
		recordVFSCacheNodeID(nodeID)
	} else {
		// Backing file exists, just open it
		loopDevice, err := setupLoopDevice(VFSCacheBackingFile)
		if err != nil {
			return "", fmt.Errorf("failed to setup loop device: %w", err)
		}

		if err := luksManager.OpenLUKS(loopDevice, VFSCacheMapperName, passphrase); err != nil {
			_ = detachLoopDevice(loopDevice)
			// cryptsetup only says "No key available with this passphrase",
			// which explains nothing to whoever is paged at 3am. The
			// passphrase is <luks-secret>-<nodeID>, so name both inputs and
			// the way out.
			return "", fmt.Errorf("failed to open the encrypted VFS cache at %s: %w.\n"+
				"The passphrase is derived from the LUKS secret and this node's id (currently %q). "+
				"It fails when the secret was regenerated (helm reinstall with secret.create=true), "+
				"the node was renamed, or the chart now points at a different secret.\n"+
				"To recover unuploaded writes, open it manually with the ORIGINAL passphrase "+
				"(`printf '%%s-%%s' <secret> <old-node-id> | cryptsetup luksOpen %s tmp -`) and copy them out; "+
				"otherwise move %s aside and the driver will recreate it — losing only data not yet uploaded to S3",
				VFSCacheBackingFile, err, nodeID, VFSCacheBackingFile, VFSCacheBackingFile)
		}

		// Resize if the configured size grew (e.g. Helm value changed).
		// The filesystem is not yet mounted so resize2fs can run safely.
		if err := resizeVFSCacheIfNeeded(loopDevice, cacheSize, passphrase, luksManager); err != nil {
			klog.Warningf("Failed to resize VFS cache, continuing with existing size: %v", err)
		}
	}

	// Mount the encrypted cache
	mappedDevice := luksManager.GetMappedDevicePath(VFSCacheMapperName)
	if err := mountFilesystem(mappedDevice, mountPath); err != nil {
		_ = luksManager.CloseLUKS(VFSCacheMapperName)
		return "", fmt.Errorf("failed to mount VFS cache filesystem: %w", err)
	}

	if err := pinAndVerifyCacheDir(mountPath); err != nil {
		return "", err
	}

	klog.Infof("Successfully set up encrypted VFS cache at %s", mountPath)
	return mountPath, nil
}

// pinAndVerifyCacheDir points rclone's cache at the encrypted volume and proves
// the volume is mounted there. Runs on every path out of SetupVFSCache.
func pinAndVerifyCacheDir(mountPath string) error {
	// Pin rather than inherit: rclone derives the cache dir from
	// os.UserCacheDir() and silently falls back to os.TempDir() if HOME and
	// XDG_CACHE_HOME are unset, putting plaintext outside this LUKS volume.
	if err := config.SetCacheDir(mountPath); err != nil {
		return fmt.Errorf("failed to pin rclone cache dir to %s: %w", mountPath, err)
	}
	if got := config.GetCacheDir(); got != mountPath {
		return fmt.Errorf("rclone cache dir is %s, not the encrypted volume at %s", got, mountPath)
	}
	return VerifyCacheEncrypted()
}

// VerifyCacheEncrypted errors unless the cache path is a distinct mount from
// its parent, i.e. the LUKS volume really is mounted there.
func VerifyCacheEncrypted() error { return verifyCacheEncryptedAt(VFSCacheBasePath) }

// verifyCacheEncryptedAt compares st_dev with the parent's: no mount table,
// so a stale one cannot fool it.
func verifyCacheEncryptedAt(path string) error {
	var cache, parent syscall.Stat_t
	if err := syscall.Stat(path, &cache); err != nil {
		return fmt.Errorf("cannot stat the VFS cache dir %s: %w", path, err)
	}
	if err := syscall.Stat(filepath.Dir(path), &parent); err != nil {
		return fmt.Errorf("cannot stat %s: %w", filepath.Dir(path), err)
	}
	if cache.Dev == parent.Dev {
		return fmt.Errorf("the encrypted VFS cache is NOT mounted at %s (same device as its parent): "+
			"cached file contents would be written to this node's disk in plaintext", path)
	}
	return nil
}

// TeardownVFSCache unmounts and closes the encrypted VFS cache
func TeardownVFSCache() error {
	mountPath := VFSCacheBasePath
	luksManager := luks.NewLUKSManager()

	klog.Infof("Tearing down encrypted VFS cache")

	// Unmount the filesystem
	if err := unmountFilesystem(mountPath); err != nil {
		klog.Warningf("Failed to unmount VFS cache filesystem: %v", err)
	}

	// Close LUKS
	if err := luksManager.CloseLUKS(VFSCacheMapperName); err != nil {
		klog.Warningf("Failed to close LUKS VFS cache: %v", err)
	}

	// Find and detach loop device
	loopDevice, err := findLoopDevice(VFSCacheBackingFile)
	if err == nil && loopDevice != "" {
		_ = detachLoopDevice(loopDevice)
	}

	klog.Infof("Successfully tore down encrypted VFS cache")
	return nil
}

// isVFSCacheMounted checks if the VFS cache is already mounted
func isVFSCacheMounted(mountPath string) bool {
	cmd := exec.Command("mountpoint", "-q", mountPath)
	return cmd.Run() == nil
}

// cleanupStaleVFSCache cleans up any stale VFS cache state
func cleanupStaleVFSCache(mountPath string) {
	luksManager := luks.NewLUKSManager()
	mappedDevice := luksManager.GetMappedDevicePath(VFSCacheMapperName)

	// Unmount if stale mount exists
	if cmd := exec.Command("mountpoint", "-q", mountPath); cmd.Run() == nil {
		klog.Infof("Found stale VFS cache mount at %s, unmounting", mountPath)
		_ = exec.Command("umount", "-l", mountPath).Run()
	}

	// Close LUKS mapper if it exists
	if _, err := os.Stat(mappedDevice); err == nil {
		klog.Infof("Found stale LUKS mapper %s, closing", VFSCacheMapperName)
		_ = luksManager.CloseLUKS(VFSCacheMapperName)
	}

	// Detach loop device if attached
	if _, err := os.Stat(VFSCacheBackingFile); err == nil {
		if loopDevice, err := findLoopDevice(VFSCacheBackingFile); err == nil && loopDevice != "" {
			klog.Infof("Found stale loop device %s, detaching", loopDevice)
			_ = detachLoopDevice(loopDevice)
		}
	}
}

// resizeVFSCacheIfNeeded expands the VFS cache LUKS volume when the configured
// size is larger than the current backing file. The LUKS device must already be
// open and the filesystem must not yet be mounted when this is called.
func resizeVFSCacheIfNeeded(loopDevice string, targetSize int64, passphrase string, luksManager *luks.LUKSManager) error {
	info, err := os.Stat(VFSCacheBackingFile)
	if err != nil {
		return fmt.Errorf("failed to stat VFS cache backing file: %w", err)
	}

	currentSize := info.Size()
	if currentSize >= targetSize {
		klog.V(4).Infof("VFS cache size unchanged (%d bytes), no resize needed", currentSize)
		return nil
	}

	klog.Infof("Resizing VFS cache backing file from %d to %d bytes", currentSize, targetSize)

	// Expand the sparse backing file
	if err := os.Truncate(VFSCacheBackingFile, targetSize); err != nil {
		return fmt.Errorf("failed to expand VFS cache backing file: %w", err)
	}

	// Inform the loop device of the new file size
	if out, err := exec.Command("losetup", "-c", loopDevice).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to refresh loop device %s: %v (%s)", loopDevice, err, strings.TrimSpace(string(out)))
	}

	// Expand the LUKS container to fill the enlarged block device
	if err := luksManager.ResizeLUKS(VFSCacheMapperName, passphrase); err != nil {
		return fmt.Errorf("failed to resize LUKS VFS cache: %w", err)
	}

	// Expand the ext4 filesystem to fill the enlarged LUKS device
	mappedDevice := luksManager.GetMappedDevicePath(VFSCacheMapperName)
	if out, err := exec.Command("resize2fs", mappedDevice).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to resize VFS cache filesystem: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	klog.Infof("Successfully resized VFS cache to %d bytes", targetSize)
	return nil
}

// Helper functions (these might already exist in syncmanager.go, but included for completeness)

func setupLoopDevice(backingFile string) (string, error) {
	cmd := exec.Command("losetup", "-f", "--show", backingFile)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("losetup failed: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

func findLoopDevice(backingFile string) (string, error) {
	cmd := exec.Command("losetup", "-j", backingFile)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(output))
	if line == "" {
		return "", fmt.Errorf("no loop device found")
	}
	parts := strings.Split(line, ":")
	if len(parts) > 0 {
		return parts[0], nil
	}
	return "", fmt.Errorf("could not parse losetup output")
}

func detachLoopDevice(loopDevice string) error {
	cmd := exec.Command("losetup", "-d", loopDevice)
	return cmd.Run()
}

func formatExt4(device string) error {
	cmd := exec.Command("mkfs.ext4", "-F", device)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func mountFilesystem(device, mountPath string) error {
	// Check if already mounted
	data, _ := os.ReadFile("/proc/mounts")
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == mountPath {
			klog.Infof("Filesystem already mounted at %s", mountPath)
			return nil
		}
	}

	cmd := exec.Command("mount", device, mountPath)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func unmountFilesystem(mountPath string) error {
	cmd := exec.Command("umount", mountPath)
	return cmd.Run()
}

// GetVFSCacheSize returns the configured VFS cache size in bytes
func GetVFSCacheSize() int64 {
	return vfsCacheSize
}

const drainPendingDir = VFSCacheBasePath + "/.drain-pending"

// SaveDrainPending writes a marker file indicating that volumeID has an
// in-progress drain. The marker survives process restarts because it lives
// inside the LUKS-backed VFS cache filesystem.
func SaveDrainPending(volumeID string) {
	if err := os.MkdirAll(drainPendingDir, 0700); err != nil {
		klog.Warningf("Volume %s: failed to create drain-pending dir: %v", volumeID, err)
		return
	}
	path := filepath.Join(drainPendingDir, volumeID)
	if err := os.WriteFile(path, []byte{}, 0600); err != nil {
		klog.Warningf("Volume %s: failed to write drain-pending marker: %v", volumeID, err)
	}
}

// ClearDrainPending removes the drain-pending marker for volumeID.
func ClearDrainPending(volumeID string) {
	path := filepath.Join(drainPendingDir, volumeID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		klog.Warningf("Volume %s: failed to remove drain-pending marker: %v", volumeID, err)
	}
}

// ListDrainPending returns the volumeIDs that have a persistent drain-pending
// marker, indicating an incomplete drain from a previous driver instance.
func ListDrainPending() []string {
	entries, err := os.ReadDir(drainPendingDir)
	if err != nil {
		if !os.IsNotExist(err) {
			klog.Warningf("Failed to read drain-pending dir: %v", err)
		}
		return nil
	}
	result := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			result = append(result, e.Name())
		}
	}
	return result
}

// LoadVFSNameMap reads the persisted volumeID→vfsName mapping from disk.
// Returns an empty map if the file does not exist or cannot be read.
func LoadVFSNameMap() map[string]string {
	data, err := os.ReadFile(vfsNameMapFile)
	if err != nil {
		if !os.IsNotExist(err) {
			klog.Warningf("Failed to read VFS name map file %s: %v", vfsNameMapFile, err)
		}
		return make(map[string]string)
	}

	m := make(map[string]string)
	if err := json.Unmarshal(data, &m); err != nil {
		klog.Warningf("Failed to parse VFS name map file %s: %v", vfsNameMapFile, err)
		return make(map[string]string)
	}
	return m
}

// writeVFSNameMap writes the volumeID→vfsName mapping to disk.
// Caller must hold vfsNameMapMu.
func writeVFSNameMap(m map[string]string) {
	data, err := json.Marshal(m)
	if err != nil {
		klog.Warningf("Failed to marshal VFS name map: %v", err)
		return
	}
	if err := os.WriteFile(vfsNameMapFile, data, 0600); err != nil {
		klog.Warningf("Failed to write VFS name map file %s: %v", vfsNameMapFile, err)
	}
}

// SaveVFSName persists a volumeID→vfsName mapping to the JSON file.
func SaveVFSName(volumeID, vfsName string) {
	vfsNameMapMu.Lock()
	defer vfsNameMapMu.Unlock()

	m := LoadVFSNameMap()
	m[volumeID] = vfsName
	writeVFSNameMap(m)
	klog.V(4).Infof("Saved VFS name mapping: %s -> %s", volumeID, vfsName)
}

// RemoveVFSName removes a volumeID entry from the persisted mapping.
func RemoveVFSName(volumeID string) {
	vfsNameMapMu.Lock()
	defer vfsNameMapMu.Unlock()

	m := LoadVFSNameMap()
	if _, exists := m[volumeID]; !exists {
		return
	}
	delete(m, volumeID)
	writeVFSNameMap(m)
	klog.V(4).Infof("Removed VFS name mapping for volume %s", volumeID)
}

// CleanupOrphanedVFSCacheDirs removes cache dirs no live volume needs. isActive
// is consulted per candidate, so the persisted map alone never causes a delete.
func CleanupOrphanedVFSCacheDirs(isActive func(volumeID string) bool) {
	vfsNameMapMu.Lock()
	defer vfsNameMapMu.Unlock()

	m := LoadVFSNameMap()

	changed := false
	for volumeID, vfsName := range m {
		if isActive(volumeID) {
			continue
		}
		klog.Infof("Cleaning up orphaned VFS cache for volume %s: %s/vfs/%s", volumeID, VFSCacheBasePath, vfsName)
		removeVFSCacheDirsAt(VFSCacheBasePath, vfsName)
		delete(m, volumeID)
		changed = true
		klog.Infof("Successfully removed orphaned VFS cache for volume %s", volumeID)
	}

	if changed {
		writeVFSNameMap(m)
	}

	sweepUnmappedVFSCacheDirsAt(VFSCacheBasePath, m, isActive)
}

// removeVFSCacheDirsAt deletes a VFS name's data and metadata directories.
func removeVFSCacheDirsAt(base, vfsName string) {
	cacheDir := fmt.Sprintf("%s/vfs/%s", base, vfsName)
	metaDir := fmt.Sprintf("%s/vfsMeta/%s", base, vfsName)
	if err := os.RemoveAll(cacheDir); err != nil {
		klog.Warningf("Failed to remove VFS cache dir %s: %v", cacheDir, err)
	}
	if err := os.RemoveAll(metaDir); err != nil {
		klog.Warningf("Failed to remove VFS meta dir %s: %v", metaDir, err)
	}
}

// sweepUnmappedVFSCacheDirsAt reclaims cache dirs the name map does not point
// at: abandoned generations and leftovers from a lost map file. Dirs holding
// unuploaded items are the only copy of those writes, so they are kept.
func sweepUnmappedVFSCacheDirsAt(base string, m map[string]string, isActive func(volumeID string) bool) {
	entries, err := os.ReadDir(filepath.Join(base, "vfs"))
	if err != nil {
		if !os.IsNotExist(err) {
			klog.Warningf("Could not scan VFS cache dirs: %v", err)
		}
		return
	}

	mapped := make(map[string]bool, len(m))
	for _, name := range m {
		mapped[name] = true
	}

	for _, e := range entries {
		if !e.IsDir() || mapped[e.Name()] {
			continue
		}
		vfsName := e.Name()
		volumeID := volumeIDOfVFSName(vfsName)
		active := isActive(volumeID)

		if active && vfsName == volumeID {
			continue // live volume's current cache, just not in the map yet
		}
		if hasDirtyCacheItemsAt(base, vfsName) {
			klog.Errorf("VFS cache dir %s/vfs/%s belongs to no live mount but still holds items that were never "+
				"uploaded; keeping it. Recover them manually — nothing else will.", base, vfsName)
			continue
		}
		if active {
			klog.Infof("Reclaiming abandoned VFS cache generation %s of live volume %s (nothing unuploaded left)",
				vfsName, volumeID)
		} else {
			klog.Infof("Reclaiming VFS cache dir %s of deleted volume %s", vfsName, volumeID)
		}
		removeVFSCacheDirsAt(base, vfsName)
	}
}

// hasDirtyCacheItemsAt reports unuploaded writes, erring towards "dirty": an
// unreadable metadata tree is no evidence the data reached S3.
func hasDirtyCacheItemsAt(base, vfsName string) bool {
	metaRoot := filepath.Join(base, "vfsMeta", vfsName)
	dirty := false
	err := filepath.WalkDir(metaRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if isCacheItemDirty(path) {
			dirty = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		klog.Warningf("Could not check %s for unuploaded items (%v); treating it as dirty", metaRoot, err)
		return true
	}
	return dirty
}
