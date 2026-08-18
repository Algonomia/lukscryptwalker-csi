package rclone

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"k8s.io/klog"
)

// mountGeneration makes each mount's VFS options unique so rclone's vfs.New
// never reuses a leaked VFS (same fs + identical options) from an unclean
// unmount — a reused cancelled VFS serves EIO on every open.
var mountGeneration atomic.Int64

// Budgets for librclone calls, which cannot be cancelled: past these we stop
// waiting rather than block a CSI handler or the checker.
const (
	rpcMountTimeout   = 2 * time.Minute
	rpcUnmountTimeout = 2 * time.Minute
	rpcListTimeout    = 15 * time.Second

	// VFS.Stats walks the cached directory tree holding every Dir lock, and
	// those locks are held across backend listings — so this blocks for as long
	// as a listing takes. Callers with a deadline clamp it to what they have left.
	rpcStatsTimeout = 2 * time.Minute
	// Fast-path probe: a slow answer is itself the answer, take the background path.
	rpcStatsProbeTimeout = 15 * time.Second
	// vfs/refresh re-lists the remote; minutes is normal. Nothing waits on it.
	rpcRefreshTimeout = 30 * time.Minute
)

// maxVFSGenerations bounds the fresh names one mount attempt may try; past it
// something leaks a VFS every cycle and the answer is a restart.
const maxVFSGenerations = 16

// vfsNamesFor returns a volume's rclone config names at a given generation.
// Generation 0 is the bare volumeID so existing on-disk caches keep resuming;
// later generations avoid colliding with a leaked VFS of the same name.
func vfsNamesFor(volumeID string, generation int) (vfsName, s3ConfigName, cryptConfigName string) {
	vfsName = volumeID
	if generation > 0 {
		vfsName = fmt.Sprintf("%s.g%d", volumeID, generation)
	}
	return vfsName, vfsName + "-s3", vfsName
}

// volumeIDOfVFSName strips any generation suffix. PV names carry no dots.
func volumeIDOfVFSName(vfsName string) string {
	i := strings.LastIndex(vfsName, ".g")
	if i <= 0 {
		return vfsName
	}
	if _, err := strconv.Atoi(vfsName[i+2:]); err != nil {
		return vfsName
	}
	return vfsName[:i]
}

// generationOfVFSName recovers the generation from a persisted vfsName.
func generationOfVFSName(volumeID, vfsName string) int {
	if vfsName == "" || vfsName == volumeID {
		return 0
	}
	suffix, ok := strings.CutPrefix(vfsName, volumeID+".g")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(suffix)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// VFSCacheConfig holds VFS cache and directory-metadata configuration options
type VFSCacheConfig struct {
	CacheMode         string // off, minimal, writes, full
	CacheMaxAge       string // e.g., "1h", "24h"
	CacheMaxSize      string // e.g., "10G", "100M"
	CachePollInterval string // e.g., "1m", "5m" - how often to poll for stale cache entries
	WriteBack         string // e.g., "5s", "0" for immediate
	DirCacheTime      string // cache directory listings, e.g. "5m", "1h"
	AttrTimeout       string // cache file attributes (stat), e.g. "5m", "1h"
	ChunkStreams      string // parallel chunk download streams per file, e.g. "2"
}

// DefaultVFSCacheConfig returns sensible defaults for VFS caching
func DefaultVFSCacheConfig() *VFSCacheConfig {
	return &VFSCacheConfig{
		CacheMode:         "full",
		CacheMaxAge:       "4h", // 4 hours to handle long-running operations
		CacheMaxSize:      "5G", // 5GB to handle large files
		CachePollInterval: "1m", // Poll every minute for stale cache entries
		WriteBack:         "3s", // Start uploads quickly to reduce cache pressure
		// 1h metadata caching avoids an S3 ListObjects+decrypt on every stat/
		// readdir; safe for these RWO/single-writer volumes. Override per SC.
		DirCacheTime: "1h",
		AttrTimeout:  "1h",
	}
}

// MountManager handles rclone mount operations for encrypted S3 volumes
type MountManager struct {
	s3Config        *S3Config
	cryptConfig     *CryptConfig
	vfsConfig       *VFSCacheConfig
	volumeID        string
	mountPoint      string
	s3BasePath      string
	mounted         bool
	vfsName         string        // Deterministic VFS name derived from volumeID
	s3ConfigName    string        // Named rclone config for S3 backend
	cryptConfigName string        // Named rclone config for crypt layer
	uid             *int64        // UID for FUSE mount (from fsGroup)
	gid             *int64        // GID for FUSE mount (from fsGroup)
	generation      int           // VFS name generation; bumped when a leaked VFS holds the current name
	stopCacheMon    chan struct{} // Signals the background cache monitor to stop
	stopMonOnce     sync.Once     // Guards stopCacheMon close so it is safe to call from both Unmount and reconcile
	refreshMu       sync.Mutex    // Serializes refreshVFS calls to prevent concurrent forget/refresh races
}

// StopCacheMonitor stops the background cache monitor without unmounting.
// Idempotent. Call before dropping a stale manager so its monitor doesn't keep
// evicting a cache dir that a replacement mount now owns.
func (mm *MountManager) StopCacheMonitor() {
	if mm.stopCacheMon != nil {
		mm.stopMonOnce.Do(func() { close(mm.stopCacheMon) })
	}
}

// NewMountManager creates a new rclone mount manager
// s3PathPrefix is optional - if empty, defaults to "volumes/{volumeID}/files"
// If s3PathPrefix is provided, path becomes "{s3PathPrefix}/volumes/{volumeID}/files"
func NewMountManager(s3Config *S3Config, volumeID, mountPoint string, vfsConfig *VFSCacheConfig, s3PathPrefix string, luksPassphrase string, fsGroup *int64) (*MountManager, error) {
	if vfsConfig == nil {
		vfsConfig = DefaultVFSCacheConfig()
	}

	// Determine S3 base path - always include volumeID to ensure each volume has its own directory
	var s3BasePath string
	if s3PathPrefix == "" {
		s3BasePath = fmt.Sprintf("volumes/%s/files", volumeID)
	} else {
		s3BasePath = fmt.Sprintf("%s/volumes/%s/files", s3PathPrefix, volumeID)
	}

	// Use s3PathPrefix as salt for password2 if set, otherwise use volumeID
	// This allows users to access data externally using just passphrase + path prefix
	salt := s3PathPrefix
	if salt == "" {
		salt = volumeID
	}
	cryptConfig := DeriveRcloneCryptConfig(luksPassphrase, salt)

	// Resume on the name this volume last used: the VFS cache dir is derived
	// from it, and picking a different one would strand unuploaded writes.
	generation := generationOfVFSName(volumeID, LoadVFSNameMap()[volumeID])
	vfsName, s3ConfigName, cryptConfigName := vfsNamesFor(volumeID, generation)

	manager := &MountManager{
		s3Config:        s3Config,
		cryptConfig:     cryptConfig,
		vfsConfig:       vfsConfig,
		volumeID:        volumeID,
		mountPoint:      mountPoint,
		s3BasePath:      s3BasePath,
		vfsName:         vfsName,
		s3ConfigName:    s3ConfigName,
		cryptConfigName: cryptConfigName,
		generation:      generation,
		uid:             fsGroup,
		gid:             fsGroup,
	}

	klog.Infof("Created rclone mount manager for volume %s at %s (s3Path: %s, vfs: %s)",
		volumeID, mountPoint, s3BasePath, vfsName)
	return manager, nil
}

// ensureExclusiveVFSName guarantees no other VFS holds the name this mount will
// use: rclone only runs VFS.Shutdown on an explicit unmount, so a FUSE that died
// by itself leaves an unreachable VFS that makes every vfs/* RPC for the volume
// ambiguous. Reports whether a predecessor was found (its finalizer may still
// fire); on error the caller must not mount.
func (mm *MountManager) ensureExclusiveVFSName() (leaked bool, err error) {
	for bumps := 0; ; bumps++ {
		count, ok := mm.countRegisteredVFS()
		if ok && count == 0 {
			return leaked, nil
		}

		fsName := mm.cryptConfigName + ":"

		if !ok {
			// Cannot verify. Mounting blind is how the duplicate arises, and a
			// duplicate is permanent until restart while a fresh name costs at
			// most a cold cache. Bump once, then proceed: if vfs/list is broken
			// for good, bumping forever helps nobody.
			if bumps > 0 {
				klog.Warningf("Volume %s: still cannot verify VFS name exclusivity; mounting as %q",
					mm.volumeID, mm.cryptConfigName+":")
				return leaked, nil
			}
			klog.Warningf("Volume %s: cannot verify whether %q is free; taking a fresh generation rather than "+
				"risking a second VFS under the same name", mm.volumeID, fsName)
			mm.bumpGeneration()
			leaked = true
			continue
		}

		// Reachable through the mount registry? Then unmounting runs
		// VFS.Shutdown and the name is free again.
		leaked = true
		if UnmountDead(mm.mountPoint) {
			time.Sleep(500 * time.Millisecond)
			if count, ok = mm.countRegisteredVFS(); !ok || count == 0 {
				klog.Infof("Volume %s: shut down the previous VFS instance of %q before re-mounting",
					mm.volumeID, fsName)
				return leaked, nil
			}
		}

		if bumps >= maxVFSGenerations {
			// Mounting anyway would register a second VFS under a name we know
			// is taken, and every vfs/* RPC for this volume would answer "more
			// than one VFS active" until the driver restarts — no drain, no
			// queue observation, no safe eviction, forever. Refuse; the caller
			// retries, and the RPC watchdog restarts us if this is terminal.
			return leaked, fmt.Errorf("volume %s: %d VFS instances still registered for %q after %d generation "+
				"bumps; refusing to mount a second one under a taken name", mm.volumeID, count, fsName, bumps)
		}

		old := mm.vfsName
		mm.bumpGeneration()
		klog.Warningf("Volume %s: %d VFS instance(s) leaked under %q with no live mount to shut them down "+
			"through; mounting as %q instead so vfs/* RPCs stay unambiguous. The abandoned cache stays at "+
			"%s/vfs/%s — it is reclaimed once nothing in it is still waiting to upload",
			mm.volumeID, count, fsName, mm.cryptConfigName+":", VFSCacheBasePath, old)
	}
}

// bumpGeneration moves this mount to the next VFS name generation.
func (mm *MountManager) bumpGeneration() {
	mm.generation++
	mm.vfsName, mm.s3ConfigName, mm.cryptConfigName = vfsNamesFor(mm.volumeID, mm.generation)
}

// countRegisteredVFS returns how many VFSes rclone has under this fs name, and
// whether the answer could be obtained at all.
func (mm *MountManager) countRegisteredVFS() (int, bool) {
	result, err := RPCWithTimeout("vfs/list", map[string]interface{}{}, rpcListTimeout)
	if err != nil || result == nil || result.Output == nil {
		klog.Warningf("Volume %s: could not list active VFSes (%v); cannot verify name exclusivity", mm.volumeID, err)
		return 0, false
	}
	names, ok := result.Output["vfses"].([]interface{})
	if !ok {
		return 0, false
	}
	// Duplicates are listed as "name:[i]", singletons as "name:".
	prefix := mm.cryptConfigName + ":"
	count := 0
	for _, n := range names {
		if s, ok := n.(string); ok && (s == prefix || strings.HasPrefix(s, prefix+"[")) {
			count++
		}
	}
	return count, true
}

// Mount mounts the encrypted S3 remote at the mount point using librclone
func (mm *MountManager) Mount() error {

	if mm.mounted && mm.isMountPoint() {
		klog.Infof("Volume %s already mounted at %s", mm.volumeID, mm.mountPoint)
		return nil
	}

	klog.Infof("Mounting encrypted S3 volume %s at %s", mm.volumeID, mm.mountPoint)

	// Clean up a stale mount, with a bounded drain: this runs inside CSI handlers
	// and the checker. hadPredecessor drives the settle window below — a session
	// torn down here can still run its unmount-by-path finalizer minutes later.
	hadPredecessor := mm.isMountPoint()
	if hadPredecessor {
		klog.Warningf("Found stale mount at %s, attempting to unmount first", mm.mountPoint)
		if err := mm.unmount(ShortDrainWait); err != nil {
			klog.Warningf("Unmount failed: %v, will try mounting anyway with AllowNonEmpty", err)
		}
	}

	// Must precede everything keyed on the VFS name: it can move this mount to
	// a fresh generation when a leaked instance still holds the current one.
	if leaked, err := mm.ensureExclusiveVFSName(); err != nil {
		return err
	} else if leaked {
		hadPredecessor = true
	}

	// Quarantine unloadable cache items before rclone can serve them: one
	// corrupt item blocks its readers forever and can stall the whole node.
	ValidateVFSCache(mm.vfsName)

	// Ensure mount point exists. Bounded: MkdirAll stats the path, and if the
	// unmount above failed to detach a wedged FUSE that stat never returns.
	if err := mkdirAllBounded(mm.mountPoint, 0755, 30*time.Second); err != nil {
		return fmt.Errorf("failed to create mount point: %w", err)
	}

	// Create named rclone configs (deterministic names for stable VFS cache dirs)
	if err := CreateNamedS3Config(mm.s3ConfigName, mm.s3Config); err != nil {
		return fmt.Errorf("failed to create S3 config: %w", err)
	}

	s3RemotePath := fmt.Sprintf("%s:%s/%s", mm.s3ConfigName, mm.s3Config.Bucket, mm.s3BasePath)
	if err := CreateNamedCryptConfig(mm.cryptConfigName, s3RemotePath, mm.cryptConfig); err != nil {
		DeleteNamedConfigs(mm.s3ConfigName)
		return fmt.Errorf("failed to create crypt config: %w", err)
	}

	// Fallback only if a StorageClass passes an invalid duration; matches the 1h
	// DefaultVFSCacheConfig.
	const defaultCacheNs = int64(3600000000000) // 1h
	mountOpt := map[string]interface{}{
		"AllowOther":    true,
		"AllowNonEmpty": true,
		"DirCacheTime":  durationOrDefault(mm.vfsConfig.DirCacheTime, defaultCacheNs),
		"AttrTimeout":   durationOrDefault(mm.vfsConfig.AttrTimeout, defaultCacheNs),
	}

	// Build VFS options
	vfsOpt := mm.buildVFSOpt()

	// Set UID/GID on the FUSE mount so files appear owned by the pod's fsGroup,
	// allowing non-root containers to read, write, and delete files.
	// UID/GID are VFS options: librclone silently drops unknown mountOpt keys,
	// so setting them there leaves the mount root-owned (mode 0750) and any
	// non-root pod gets EACCES.
	if mm.uid != nil {
		vfsOpt["UID"] = uint32(*mm.uid)
		klog.Infof("Setting FUSE mount UID to %d for volume %s", *mm.uid, mm.volumeID)
	}
	if mm.gid != nil {
		vfsOpt["GID"] = uint32(*mm.gid)
		klog.Infof("Setting FUSE mount GID to %d for volume %s", *mm.gid, mm.volumeID)
	}

	// Call mount/mount RPC using the named crypt remote
	params := map[string]interface{}{
		"fs":         mm.cryptConfigName + ":",
		"mountPoint": mm.mountPoint,
		"mountOpt":   mountOpt,
		"vfsOpt":     vfsOpt,
	}

	klog.Infof("Calling mount/mount RPC for volume %s", mm.volumeID)

	_, err := RPCWithTimeout("mount/mount", params, rpcMountTimeout)
	if err != nil {
		DeleteNamedConfigs(mm.cryptConfigName, mm.s3ConfigName)
		if IsRPCTimeout(err) {
			// The call holds rclone's global mount lock and is still running:
			// nothing here can mount or unmount until it returns. Say so — the
			// RPC watchdog restarts the plugin if it never does.
			return fmt.Errorf("mount of volume %s did not complete within %s and still holds rclone's global "+
				"mount lock: %w", mm.volumeID, rpcMountTimeout, err)
		}
		return fmt.Errorf("failed to mount: %w", err)
	}

	// The /dev/fuse fd arrives from fusermount3 via SCM_RIGHTS without
	// CLOEXEC: exec'd children (umount, nsenter…) inherit it, and one stuck
	// child keeps the connection alive past our death — wedging the mount in
	// D-state for every accessor instead of failing fast with ENOTCONN.
	setFuseFdsCloexec()

	// Persist the vfsName mapping for orphan cleanup across restarts
	SaveVFSName(mm.volumeID, mm.vfsName)

	mm.warnOnDuplicateVFS()

	// Verify the FUSE mount is live and responding BEFORE declaring success.
	// A mount RPC can return while the FUSE daemon is not serving yet, or land
	// somewhere the host cannot see; treating either as success is what let
	// consumers bind the empty staging directory underneath and write plaintext
	// to the node's root disk. Fail instead — the caller retries.
	if err := mm.waitForMountReady(hadPredecessor); err != nil {
		klog.Errorf("Volume %s: mount did not become usable, tearing it back down: %v", mm.volumeID, err)
		// Unconditionally through the mount registry, not mm.unmount: the whole
		// point is to run VFS.Shutdown so this half-mount does not leak a VFS
		// under our name and make the retry ambiguous.
		mm.StopCacheMonitor()
		UnmountDead(mm.mountPoint)
		DeleteNamedConfigs(mm.cryptConfigName, mm.s3ConfigName)
		return fmt.Errorf("mount of volume %s did not become usable: %w", mm.volumeID, err)
	}

	mm.mounted = true
	klog.Infof("Successfully mounted encrypted S3 volume %s at %s", mm.volumeID, mm.mountPoint)

	mm.stopCacheMon = make(chan struct{})
	go mm.cacheMonitor()
	go mm.dirCacheWarmer()

	// Stale cache from unclean shutdown: async refresh reconciles rclone's
	// in-memory state with S3 while dirty files remain available for re-upload.
	if mm.hasStaleVFSCache() {
		klog.Infof("Volume %s: stale VFS cache detected, triggering async VFS refresh", mm.volumeID)
		go mm.refreshVFS()
	}

	return nil
}

// buildVFSOpt builds VFS options for the mount
func (mm *MountManager) buildVFSOpt() map[string]interface{} {
	vfsOpt := map[string]interface{}{
		"ReadChunkSize":      33554432, // 32M in bytes
		"ReadChunkSizeLimit": -1,       // off
		// 8M: the in-memory read-ahead is per OPEN FILE and mostly redundant
		// with cache-mode=full's chunk cache — 32M OOMed multi-volume nodes.
		"BufferSize": 8388608,
		"Links":      true, // Enable symlink support
	}

	// Set VFS disk space total size to match the LUKS VFS cache volume size
	// This helps rclone manage disk space within the allocated encrypted volume
	vfsOpt["DiskSpaceTotalSize"] = GetVFSCacheSize()

	// Cache mode
	if mm.vfsConfig.CacheMode != "" {
		// Map string to CacheMode value
		switch mm.vfsConfig.CacheMode {
		case "off":
			vfsOpt["CacheMode"] = 0
		case "minimal":
			vfsOpt["CacheMode"] = 1
		case "writes":
			vfsOpt["CacheMode"] = 2
		case "full":
			vfsOpt["CacheMode"] = 3
		}
	}

	// Cache max age (parse duration string to nanoseconds)
	if mm.vfsConfig.CacheMaxAge != "" {
		if ns, err := parseDurationToNs(mm.vfsConfig.CacheMaxAge); err == nil {
			vfsOpt["CacheMaxAge"] = ns
		}
	}

	// Cache max size (parse size string to bytes)
	if mm.vfsConfig.CacheMaxSize != "" {
		if bytes, err := parseSizeToBytes(mm.vfsConfig.CacheMaxSize); err == nil {
			vfsOpt["CacheMaxSize"] = bytes
		}
	}

	// Write back delay
	if mm.vfsConfig.WriteBack != "" {
		if ns, err := parseDurationToNs(mm.vfsConfig.WriteBack); err == nil {
			vfsOpt["WriteBack"] = ns
		}
	}

	// Per-volume download-stream budget: caps how much memory and bandwidth
	// one volume's reads can claim from the shared process.
	if mm.vfsConfig.ChunkStreams != "" {
		if n, err := strconv.Atoi(mm.vfsConfig.ChunkStreams); err == nil && n >= 0 {
			vfsOpt["ChunkStreams"] = n
		}
	}

	// Cache poll interval - how often to check for stale cache entries.
	// Always set, with a per-mount nanosecond uniquifier: identical options
	// would make vfs.New reuse a leaked VFS from an unclean unmount, whose
	// cancelled context serves EIO on every open.
	pollNs := int64(60 * time.Second) // rclone default 1m
	if mm.vfsConfig.CachePollInterval != "" {
		if ns, err := parseDurationToNs(mm.vfsConfig.CachePollInterval); err == nil {
			pollNs = ns
		}
	}
	vfsOpt["CachePollInterval"] = pollNs + mountGeneration.Add(1)

	return vfsOpt
}

// Drain budgets: teardown may wait to avoid aborting uploads mid-stream, but
// pre-mount cleanup runs inside CSI handlers and must not stall for hours.
const (
	maxDrainWait = 6 * time.Hour
	// ShortDrainWait is the budget for callers that already know the queue is
	// empty, or that must not block whatever else is waiting on them.
	ShortDrainWait = 60 * time.Second
)

// Unmount unmounts the S3 volume using librclone, waiting for pending uploads
// to drain first.
func (mm *MountManager) Unmount() error {
	return mm.unmount(maxDrainWait)
}

// UnmountWithin unmounts, waiting at most budget for the write-back queue.
func (mm *MountManager) UnmountWithin(budget time.Duration) error {
	return mm.unmount(budget)
}

// unmount unmounts the volume, spending at most drainBudget waiting for the
// write-back queue to empty.
func (mm *MountManager) unmount(drainBudget time.Duration) error {

	if !mm.mounted && !mm.isMountPoint() {
		klog.Infof("Volume %s not mounted, skipping unmount", mm.volumeID)
		return nil
	}

	klog.Infof("Unmounting encrypted S3 volume %s from %s", mm.volumeID, mm.mountPoint)

	mm.StopCacheMonitor()

	// Drain before unmounting: mount/unmount cancels rclone's VFS context, which
	// aborts all in-flight uploads mid-stream and leaves partial objects in S3.
	// false → drain unconfirmed; keep VFS cache so rclone can retry on next mount.
	drained := mm.waitForPendingUploads(drainBudget)

	params := map[string]interface{}{"mountPoint": mm.mountPoint}
	_, err := RPCWithTimeout("mount/unmount", params, rpcUnmountTimeout)
	if err != nil {
		if strings.Contains(err.Error(), "mount not found") {
			// rclone self-unmounted (e.g. VFS error); already gone, not a failure.
			klog.Infof("Volume %s: mount already gone when calling mount/unmount — rclone self-unmounted", mm.volumeID)
		} else {
			// The RPC ran VFS.Shutdown before the kernel unmount failed; only the
			// kernel detach remains. Never fall back to mount/unmountall — it
			// tears down every volume's mount on the node.
			klog.Warningf("Volume %s: mount/unmount RPC failed: %v — detaching kernel mount directly", mm.volumeID, err)
			if out, uerr := exec.Command("umount", "-l", mm.mountPoint).CombinedOutput(); uerr != nil {
				klog.Warningf("Volume %s: umount -l %s failed: %v (%s)", mm.volumeID, mm.mountPoint, uerr, strings.TrimSpace(string(out)))
			}
		}
	}

	DeleteNamedConfigs(mm.cryptConfigName, mm.s3ConfigName)

	if drained {
		mm.cleanupVFSCacheDir()
	} else {
		klog.Infof("Volume %s: preserving VFS cache for retry on next mount", mm.volumeID)
	}

	mm.mounted = false

	klog.Infof("Successfully unmounted encrypted S3 volume %s", mm.volumeID)
	return nil
}

// cacheFSFreeFraction returns the free-space fraction of the shared VFS cache
// filesystem, or ok=false when it cannot be determined.
func cacheFSFreeFraction() (float64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(VFSCacheBasePath, &st); err != nil || st.Blocks == 0 {
		return 0, false
	}
	return float64(st.Bavail) / float64(st.Blocks), true
}

// setFuseFdsCloexec marks every /dev/fuse fd in this process close-on-exec.
// SCM_RIGHTS-received fds bypass Go's CLOEXEC convention; called after each
// mount so no exec'd child can extend a FUSE connection past our death.
func setFuseFdsCloexec() {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return
	}
	for _, e := range entries {
		target, err := os.Readlink("/proc/self/fd/" + e.Name())
		if err != nil || target != "/dev/fuse" {
			continue
		}
		fd, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFD, syscall.FD_CLOEXEC); errno != 0 {
			klog.Warningf("Failed to set CLOEXEC on fuse fd %d: %v", fd, errno)
		}
	}
}

// UnmountDead tears down a dead/zombie librclone mount without a MountManager.
// Unlike a bare `umount -l`, mount/unmount runs VFS.Shutdown, dropping the VFS
// from rclone's active registry and stopping its cache writers. Returns true
// when a live session was found — its Wait() finalizer fires asynchronously
// and will unmount whatever is at the path when it runs, so the caller must
// let it fire against an empty path before mounting fresh.
func UnmountDead(mountPoint string) bool {
	if _, err := RPCWithTimeout("mount/unmount", map[string]interface{}{"mountPoint": mountPoint}, rpcUnmountTimeout); err != nil {
		if strings.Contains(err.Error(), "mount not found") {
			klog.V(4).Infof("UnmountDead %s: no live rclone mount entry", mountPoint)
			return false
		}
		// VFS.Shutdown still ran; the caller's umount -l completes the detach.
		klog.Warningf("UnmountDead %s: mount/unmount RPC failed: %v", mountPoint, err)
		return true
	}
	klog.Infof("UnmountDead %s: old librclone session shut down cleanly", mountPoint)
	return true
}

// waitForPendingUploads polls vfs/stats until the write-back queue is empty or
// maxWait elapses. Returns true only when the queue is confirmed empty; false
// otherwise (the caller must preserve the local VFS cache for retry on next
// mount). RPC failures are retried while the FUSE mount is alive — a lost RC
// connection is not evidence the queue is empty.
func (mm *MountManager) waitForPendingUploads(maxWait time.Duration) bool {
	if maxWait <= 0 {
		return false
	}
	klog.Infof("Waiting for pending uploads to complete for volume %s (budget %s)", mm.volumeID, maxWait)

	deadline := time.Now().Add(maxWait)

	writeBackWait := 5
	if mm.vfsConfig.WriteBack != "" {
		if ns, err := parseDurationToNs(mm.vfsConfig.WriteBack); err == nil {
			writeBackWait = int(ns / 1e9)
		}
	}
	writeBackWait += 2
	flush := min(time.Duration(writeBackWait)*time.Second, maxWait)
	klog.Infof("Waiting %s for write-back to flush for volume %s", flush, mm.volumeID)
	time.Sleep(flush)

	pollInterval := 2 * time.Second
	logInterval := 30 * time.Second
	lastLog := time.Now()
	consecutiveRPCFailures := 0

	fsName := mm.cryptConfigName + ":"
	for time.Now().Before(deadline) {
		// Self-unmount: rclone tore down the FUSE and cancelled all in-flight uploads.
		if !mm.isMountPoint() {
			klog.Errorf("Volume %s: rclone FUSE mount disappeared while waiting for uploads to drain — "+
				"in-flight uploads were cancelled; local VFS cache will be preserved for retry on next mount", mm.volumeID)
			return false
		}

		// Never let one stats call outlive the drain budget the caller was
		// given: ShortDrainWait exists so CSI handlers and the checker are not
		// held up, and a 2-minute ceiling would quietly break that promise.
		budget := min(rpcStatsTimeout, time.Until(deadline))
		if budget <= 0 {
			break
		}
		result, err := RPCWithTimeout("vfs/stats", map[string]interface{}{"fs": fsName}, budget)
		if err != nil {
			// "no VFS found" is terminal, not transient: the kernel mount exists
			// but this librclone instance never created its VFS (orphaned after a
			// driver restart). Retrying can never succeed, so stop and preserve
			// the on-disk cache for the next mount to resume.
			if isNoVFSError(err) {
				klog.Warningf("Volume %s: mount has no VFS in this driver instance (orphaned after a restart); "+
					"stopping drain, preserving cache for re-mount", mm.volumeID)
				return false
			}
			// Queue state unobservable while an orphaned VFS shares the name;
			// retrying can't resolve it. Preserve the cache — the next mount's
			// reload re-uploads anything still dirty.
			if isAmbiguousVFSError(err) {
				klog.Warningf("Volume %s: duplicate VFS registered for this fs (leaked orphan); "+
					"stopping drain, preserving cache for re-mount: %v", mm.volumeID, err)
				return false
			}
			consecutiveRPCFailures++
			if consecutiveRPCFailures == 1 || consecutiveRPCFailures%5 == 0 {
				klog.Warningf("Volume %s: vfs/stats RPC failing (consecutive failures: %d): %v — "+
					"retrying to protect locally cached data", mm.volumeID, consecutiveRPCFailures, err)
			}
			time.Sleep(pollInterval)
			continue
		}
		consecutiveRPCFailures = 0

		inProgress, queued := int64(0), int64(0)
		if result != nil && result.Output != nil {
			if dc, ok := result.Output["diskCache"].(map[string]interface{}); ok {
				if v, ok := dc["uploadsInProgress"].(float64); ok {
					inProgress = int64(v)
				}
				if v, ok := dc["uploadsQueued"].(float64); ok {
					queued = int64(v)
				}
			}
		}

		if inProgress == 0 && queued == 0 {
			klog.Infof("Volume %s: upload queue empty, proceeding with unmount", mm.volumeID)
			return true
		}
		if time.Since(lastLog) >= logInterval {
			klog.Infof("Volume %s: waiting for %d uploads in progress, %d queued", mm.volumeID, inProgress, queued)
			lastLog = time.Now()
		}
		time.Sleep(pollInterval)
	}

	klog.Warningf("Volume %s: upload drain timed out after %s — local VFS cache will be preserved for retry on next mount", mm.volumeID, maxWait)
	return false
}

// IsUploadQueueEmpty does a single non-blocking poll of vfs/stats.
// Returns true if there are no uploads in progress or queued.
// Use waitForPendingUploads to block until the queue drains.
func (mm *MountManager) IsUploadQueueEmpty() bool {
	if !mm.isMountPoint() {
		return true
	}
	// Deliberately the short budget: this runs inside NodeUnstageVolume to
	// decide whether the fast path is available. If stats cannot answer
	// promptly the volume is busy enough that the background drain is the
	// right call anyway — waiting longer would only delay kubelet.
	result, err := RPCWithTimeout("vfs/stats", map[string]interface{}{"fs": mm.cryptConfigName + ":"}, rpcStatsProbeTimeout)
	if err != nil {
		// Orphaned mount (no VFS in this instance): nothing to drain here, take
		// the fast unmount path rather than starting an endless background drain.
		// Same for a duplicate-VFS name clash: the queue is unobservable, and the
		// fast path's Unmount preserves the cache for reload to re-upload.
		if isNoVFSError(err) || isAmbiguousVFSError(err) {
			return true
		}
		return false // assume work pending on other RPC errors
	}
	if result != nil && result.Output != nil {
		if dc, ok := result.Output["diskCache"].(map[string]interface{}); ok {
			inProgress, queued := int64(0), int64(0)
			if v, ok := dc["uploadsInProgress"].(float64); ok {
				inProgress = int64(v)
			}
			if v, ok := dc["uploadsQueued"].(float64); ok {
				queued = int64(v)
			}
			return inProgress == 0 && queued == 0
		}
	}
	return false
}

// uploadQueueConfirmedEmpty reports whether vfs/stats definitively shows an
// empty upload queue. Any error (no VFS, duplicate VFS, RPC failure) or a
// missing diskCache section is NOT confirmation — callers gating destructive
// actions must treat the queue as pending.
func (mm *MountManager) uploadQueueConfirmedEmpty() bool {
	if !mm.isMountPoint() {
		return false
	}
	// The full budget: this gates cache eviction from a background monitor, so
	// nothing is waiting on it, and a premature "cannot confirm" means the
	// cache is never evicted and grows until the disk-pressure shedding fires.
	result, err := RPCWithTimeout("vfs/stats", map[string]interface{}{"fs": mm.cryptConfigName + ":"}, rpcStatsTimeout)
	if err != nil || result == nil || result.Output == nil {
		return false
	}
	dc, ok := result.Output["diskCache"].(map[string]interface{})
	if !ok {
		return false
	}
	inProgress, ok1 := dc["uploadsInProgress"].(float64)
	queued, ok2 := dc["uploadsQueued"].(float64)
	return ok1 && ok2 && inProgress == 0 && queued == 0
}

// isMountPoint reports whether our mount is live in the HOST namespace — the
// only view that matches what consumers see. Our own /proc/mounts can retain
// entries the host has dropped, which makes the driver believe dead volumes
// are healthy and suppresses all self-healing.
func (mm *MountManager) isMountPoint() bool {
	fsType, ok := HostMounts()[mm.mountPoint]
	if ok {
		klog.V(4).Infof("Found mount at %s (%s)", mm.mountPoint, fsType)
		return true
	}
	return false
}

func (mm *MountManager) hasStaleVFSCache() bool {
	if mm.vfsName == "" {
		return false
	}
	cacheDir := fmt.Sprintf("%s/vfs/%s", VFSCacheBasePath, mm.vfsName)
	metaDir := fmt.Sprintf("%s/vfsMeta/%s", VFSCacheBasePath, mm.vfsName)
	_, err1 := os.Stat(cacheDir)
	_, err2 := os.Stat(metaDir)
	return err1 == nil || err2 == nil
}

// refreshVFS forces rclone to reconcile its VFS state against the S3 remote.
// This clears stale in-memory items and re-reads directory listings from S3,
// so that missing cache files trigger re-downloads instead of errors.
// Serialized with a mutex to prevent concurrent forget/refresh races from
// the cache monitor and mount paths.
func (mm *MountManager) refreshVFS() {
	mm.refreshMu.Lock()
	defer mm.refreshMu.Unlock()

	// Forget in-memory directory cache so rclone drops any stale Items
	if _, err := RPCWithTimeout("vfs/forget", map[string]interface{}{"fs": mm.cryptConfigName + ":"}, rpcRefreshTimeout); err != nil {
		klog.Warningf("vfs/forget failed for volume %s: %v", mm.volumeID, err)
	}

	mm.refreshTree()
}

// refreshTree re-lists the remote into the directory cache. A recursive refresh
// resolves to one ListR sweep and restamps every subdirectory it walks, so the
// whole tree is renewed by a single paginated listing. Caller holds refreshMu.
func (mm *MountManager) refreshTree() {
	refreshParams := map[string]interface{}{
		"fs":        mm.cryptConfigName + ":",
		"dir":       "",
		"recursive": "true",
	}
	start := time.Now()
	if _, err := RPCWithTimeout("vfs/refresh", refreshParams, rpcRefreshTimeout); err != nil {
		klog.Warningf("vfs/refresh failed for volume %s: %v", mm.volumeID, err)
		return
	}
	klog.V(4).Infof("Volume %s: directory cache refreshed in %s", mm.volumeID, time.Since(start))
}

// Background dir-cache renewal. Without it the cache simply expires and the
// next consumer request pays the full listing — the reason `ls` stalls after an
// idle period. Renewing early keeps that cost off the request path entirely.
const (
	// Refresh at this fraction of DirCacheTime, leaving margin for a slow listing.
	dirCacheWarmRatio = 0.8
	// Floor so a small DirCacheTime cannot turn the warmer into a listing loop.
	minDirCacheWarmInterval = time.Minute
)

// dirCacheWarmInterval returns how often to renew the listing, or 0 when
// directory caching is off and there is nothing to keep warm.
func (mm *MountManager) dirCacheWarmInterval() time.Duration {
	dirCacheTime := time.Hour // matches DefaultVFSCacheConfig
	if mm.vfsConfig.DirCacheTime != "" {
		ns, err := parseDurationToNs(mm.vfsConfig.DirCacheTime)
		if err != nil || ns <= 0 {
			return 0
		}
		dirCacheTime = time.Duration(ns)
	}
	return max(time.Duration(float64(dirCacheTime)*dirCacheWarmRatio), minDirCacheWarmInterval)
}

// dirCacheWarmer renews the directory listing just before it expires, so a
// consumer never meets a cold cache. Refresh without a preceding forget: the
// existing entries keep serving until the fresh listing replaces them.
func (mm *MountManager) dirCacheWarmer() {
	interval := mm.dirCacheWarmInterval()
	if interval <= 0 {
		return
	}
	klog.Infof("Volume %s: renewing the directory cache every %s to keep listings off the request path",
		mm.volumeID, interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-mm.stopCacheMon:
			return
		case <-ticker.C:
			// A dead or replaced mount must not have its listing renewed: that
			// is the checker's business, and the RPC would only be ambiguous.
			if !mm.IsMounted() {
				continue
			}
			mm.refreshMu.Lock()
			mm.refreshTree()
			mm.refreshMu.Unlock()
		}
	}
}

const (
	// mountReadyTimeout bounds how long a fresh mount may take to become usable.
	mountReadyTimeout = 20 * time.Second
	// How long a fresh mount must survive when a previous session existed here.
	// rclone's teardown finalizer unmounts BY PATH with no identity check and
	// fires whenever the old serve loop exits, so it can rip out our new mount.
	// Each session does this at most once, so observing it and remounting
	// converges. An accelerator, not a guarantee — the checker catches the rest.
	mountSettleWindow = 14 * time.Second
	// Must exceed the host mount-table cache TTL or every poll rereads one snapshot.
	mountSettlePoll = 2 * time.Second
)

// ErrMountRippedOut means the mount came up and was then unmounted from under
// us by a previous rclone session's teardown finalizer. Retrying is correct:
// that session has spent its one unmount.
var ErrMountRippedOut = errors.New("fresh mount was unmounted by a previous session's finalizer")

// waitForMountReady verifies the mount is visible to the host AND serving, and
// with settle, that it stays so. Invisible means consumers bind the empty
// directory underneath; visible-but-dead means they bind a dead endpoint.
func (mm *MountManager) waitForMountReady(settle bool) error {
	deadline := time.Now().Add(mountReadyTimeout)
	var lastErr error

	for time.Now().Before(deadline) {
		if err := mm.mountUsable(); err != nil {
			lastErr = err
			klog.Infof("FUSE mount not ready yet for volume %s: %v, retrying...", mm.volumeID, err)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		klog.Infof("FUSE mount verified ready for volume %s at %s", mm.volumeID, mm.mountPoint)
		if !settle {
			return nil
		}
		return mm.waitForMountToSettle()
	}

	return fmt.Errorf("FUSE mount not ready after %s for volume %s: %v", mountReadyTimeout, mm.volumeID, lastErr)
}

// waitForMountToSettle reports ErrMountRippedOut if a previous session's
// finalizer takes the fresh mount away within mountSettleWindow.
func (mm *MountManager) waitForMountToSettle() error {
	klog.Infof("Volume %s: watching the fresh mount for %s before declaring it good", mm.volumeID, mountSettleWindow)
	deadline := time.Now().Add(mountSettleWindow)
	for time.Now().Before(deadline) {
		time.Sleep(mountSettlePoll)
		isFUSE, known := HostFUSEMountState(mm.mountPoint)
		if known && !isFUSE {
			return fmt.Errorf("%w (at %s)", ErrMountRippedOut, mm.mountPoint)
		}
	}
	klog.Infof("Volume %s: mount survived the settle window", mm.volumeID)
	return nil
}

// mountUsable reports why the mount is not yet usable, or nil when it is.
func (mm *MountManager) mountUsable() error {
	isFUSE, known := HostFUSEMountState(mm.mountPoint)
	switch {
	case !known:
		return fmt.Errorf("host mount table is unreadable, cannot confirm the mount is visible to consumers")
	case !isFUSE:
		// Almost always mount propagation: the mount landed in our own
		// namespace only, so consumers would bind the empty directory
		// underneath it. Name the knob, this is otherwise unguessable.
		return fmt.Errorf("no FUSE mount at %s in the host namespace — the mount is not propagating to the "+
			"host; check that node.kubeletDir matches `readlink -f /var/lib/kubelet` on this node and that its "+
			"volumeMount is Bidirectional", mm.mountPoint)
	}
	return readDirBounded(mm.mountPoint, 5*time.Second)
}

// readDirBounded lists a directory without blocking the caller past timeout.
// Raw syscalls, not os.ReadDir — see ReadDirRaw.
func readDirBounded(path string, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		_, err := ReadDirRaw(path, 1)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("reading %s blocked for %s (wedged FUSE)", path, timeout)
	}
}

// mkdirAllBounded creates a tree without blocking on a wedged mount at the path.
func mkdirAllBounded(path string, perm os.FileMode, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- os.MkdirAll(path, perm) }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("mkdir %s blocked for %s (a wedged mount is still attached there)", path, timeout)
	}
}

// cleanupVFSCacheDir removes the on-disk VFS cache directories after unmount.
// This is safe because it is only called from Unmount() after rclone's own graceful
// unmount has flushed the write-back queue to S3. On a hard restart/crash,
// Unmount() never runs so the cache survives for rclone to resume on remount.
// Without this cleanup, cache data accumulates indefinitely because rclone's
// CacheMaxSize eviction only runs while the VFS is active.
func (mm *MountManager) cleanupVFSCacheDir() {
	if mm.vfsName == "" {
		return
	}

	cacheDir := fmt.Sprintf("%s/vfs/%s", VFSCacheBasePath, mm.vfsName)
	metaDir := fmt.Sprintf("%s/vfsMeta/%s", VFSCacheBasePath, mm.vfsName)

	if err := os.RemoveAll(cacheDir); err != nil {
		klog.Warningf("Failed to remove VFS cache dir %s: %v", cacheDir, err)
	} else {
		klog.Infof("Removed VFS cache dir %s for volume %s", cacheDir, mm.volumeID)
	}
	if err := os.RemoveAll(metaDir); err != nil {
		klog.Warningf("Failed to remove VFS meta dir %s: %v", metaDir, err)
	}

	RemoveVFSName(mm.volumeID)
}

// cacheMonitor runs in the background while the volume is mounted.
// It periodically checks the on-disk VFS cache size and, when it exceeds
// CacheMaxSize, removes the oldest already-uploaded files. This handles
// the case where rclone's built-in eviction is insufficient (e.g. long-lived
// mounts where CacheMaxAge resets on every file access).
func (mm *MountManager) cacheMonitor() {
	if mm.vfsName == "" {
		return
	}

	// Parse the configured max size; if not set, nothing to enforce
	var maxBytes int64
	if mm.vfsConfig.CacheMaxSize != "" {
		parsed, err := parseSizeToBytes(mm.vfsConfig.CacheMaxSize)
		if err != nil || parsed <= 0 {
			return
		}
		maxBytes = parsed
	} else {
		return
	}

	// Poll every CachePollInterval (default 1m), or fall back to 1m
	pollInterval := time.Minute
	if mm.vfsConfig.CachePollInterval != "" {
		if ns, err := parseDurationToNs(mm.vfsConfig.CachePollInterval); err == nil && ns > 0 {
			pollInterval = time.Duration(ns)
		}
	}

	cacheDir := fmt.Sprintf("%s/vfs/%s", VFSCacheBasePath, mm.vfsName)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-mm.stopCacheMon:
			return
		case <-ticker.C:
			mm.evictCacheIfNeeded(cacheDir, maxBytes)
		}
	}
}

// evictCacheIfNeeded checks the cache directory size and removes the oldest
// files until the total is back under maxBytes. It only removes files that
// have no open file descriptors and are not actively being transferred.
func (mm *MountManager) evictCacheIfNeeded(cacheDir string, maxBytes int64) {
	// Never evict while anything is dirty: write-back-queued files aren't
	// "transferring" yet but aren't on S3 either. Requires a CONFIRMED empty
	// queue — when vfs/stats can't answer (no VFS, duplicate VFS), evicting
	// blind can delete dirty-but-closed cache files.
	if mm.hasActiveTransfers() || !mm.uploadQueueConfirmedEmpty() {
		return
	}

	// Collect all files with their sizes and modification times
	type cachedFile struct {
		path    string
		size    int64
		modTime time.Time
	}
	var files []cachedFile
	var totalSize int64

	_ = filepath.WalkDir(cacheDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		totalSize += info.Size()
		files = append(files, cachedFile{path: path, size: info.Size(), modTime: info.ModTime()})
		return nil
	})

	// Shared-disk pressure: per-volume quotas overcommit the cache disk, so
	// when it is nearly full every volume sheds 30% of its cache — one hot
	// volume must not starve its neighbors' cache writes.
	if frac, ok := cacheFSFreeFraction(); ok && frac < 0.10 {
		if shed := totalSize * 7 / 10; shed < maxBytes {
			klog.Warningf("VFS cache disk is %.0f%% full; volume %s shedding cache from %d to %d bytes",
				(1-frac)*100, mm.volumeID, totalSize, shed)
			maxBytes = shed
		}
	}

	if totalSize <= maxBytes {
		return
	}

	klog.Infof("VFS cache for volume %s is %d bytes (limit %d), evicting oldest files",
		mm.volumeID, totalSize, maxBytes)

	// One /proc sweep for the whole pass. Probing per candidate re-read every fd
	// on the node for every file considered, which on a busy node burns real CPU
	// inside the process that also serves FUSE.
	open, realRoot, ok := openFilesUnder(cacheDir)
	if !ok {
		klog.Warningf("VFS cache for volume %s: cannot enumerate open file descriptors, skipping eviction this pass",
			mm.volumeID)
		return
	}

	// Sort oldest first
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})

	var evicted []string
	for _, f := range files {
		if totalSize <= maxBytes {
			break
		}
		// Skip files that have open file descriptors — removing them while
		// rclone holds a handle leaves the VFS item with didClose=false,
		// causing "internal error: didn't Close file" on the next access.
		if _, isOpen := open[resolveUnder(cacheDir, realRoot, f.path)]; isOpen {
			klog.V(4).Infof("Skipping eviction of open cache file %s", f.path)
			continue
		}
		if err := os.Remove(f.path); err != nil {
			klog.V(4).Infof("Could not remove cache file %s: %v", f.path, err)
			continue
		}
		totalSize -= f.size
		evicted = append(evicted, f.path)
	}

	klog.Infof("VFS cache for volume %s reduced to %d bytes", mm.volumeID, totalSize)

	// Drop the evicted files from rclone's directory cache so it stops
	// advertising cache data we just deleted — but only those. Forgetting the
	// whole fs (the previous behaviour) meant a single evicted file made the
	// next ls re-list the entire volume from S3.
	mm.forgetCachePaths(cacheDir, evicted)
}

// forgetCachePaths drops the given cache files from rclone's directory cache.
// Paths under the VFS cache dir mirror the crypt remote's decrypted layout, so
// a path relative to cacheDir is the remote path vfs/forget expects.
func (mm *MountManager) forgetCachePaths(cacheDir string, paths []string) {
	if len(paths) == 0 {
		return
	}

	mm.refreshMu.Lock()
	defer mm.refreshMu.Unlock()

	// vfs/forget takes file, file2, file3… Chunked so one large eviction does
	// not build a single enormous RPC payload.
	const perCall = 500
	params := map[string]interface{}{"fs": mm.cryptConfigName + ":"}
	n := 0
	flush := func() {
		if n == 0 {
			return
		}
		if _, err := RPCWithTimeout("vfs/forget", params, rpcRefreshTimeout); err != nil {
			klog.Warningf("Volume %s: forgetting %d evicted cache paths failed: %v", mm.volumeID, n, err)
		}
		params = map[string]interface{}{"fs": mm.cryptConfigName + ":"}
		n = 0
	}

	for _, p := range paths {
		rel, err := filepath.Rel(cacheDir, p)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		key := "file"
		if n > 0 {
			key = fmt.Sprintf("file%d", n+1)
		}
		params[key] = rel
		if n++; n >= perCall {
			flush()
		}
	}
	flush()
}

// openFilesUnder returns the canonical paths under root that some process holds
// an fd on, plus the resolved root the caller must map its paths through. The
// scan is the technique lsof uses. ok=false means the scan could not be trusted
// and nothing should be removed.
func openFilesUnder(root string) (open map[string]struct{}, realRoot string, ok bool) {
	// Kernel fd links are already fully resolved, so compare in that namespace.
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}

	fdDirs, err := filepath.Glob("/proc/*/fd")
	if err != nil {
		return nil, realRoot, false
	}

	open = make(map[string]struct{})
	for _, fdDir := range fdDirs {
		entries, err := os.ReadDir(fdDir)
		if err != nil {
			continue // process exited mid-scan; its fds went with it
		}
		for _, e := range entries {
			target, err := os.Readlink(filepath.Join(fdDir, e.Name()))
			if err != nil {
				continue
			}
			if strings.HasPrefix(target, realRoot) {
				open[target] = struct{}{}
			}
		}
	}
	return open, realRoot, true
}

// resolveUnder rewrites a path found by walking root into the resolved
// namespace that openFilesUnder reports.
func resolveUnder(root, realRoot, path string) string {
	if root == realRoot {
		return path
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.Join(realRoot, rel)
}

// hasActiveTransfers returns true if rclone is currently uploading files
func (mm *MountManager) hasActiveTransfers() bool {
	result, err := RPCWithTimeout("core/stats", map[string]interface{}{}, rpcStatsTimeout)
	if err != nil {
		return true // Assume busy on error
	}
	if result == nil || result.Output == nil {
		return false
	}
	if t, ok := result.Output["transferring"]; ok {
		if tList, ok := t.([]interface{}); ok && len(tList) > 0 {
			return true
		}
	}
	if c, ok := result.Output["checking"]; ok {
		if cList, ok := c.([]interface{}); ok && len(cList) > 0 {
			return true
		}
	}
	return false
}

// IsMounted returns whether the volume is currently mounted
func (mm *MountManager) IsMounted() bool {
	return mm.mounted && mm.isMountPoint()
}

// isNoVFSError reports whether an rclone RPC error means this librclone instance
// has no VFS for the mount — a terminal state for an orphaned kernel mount left
// by a previous driver instance, not a transient RPC failure.
func isNoVFSError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no VFS found")
}

// isAmbiguousVFSError reports whether an rclone RPC error means two VFSs share
// the fs name — a leaked orphan alongside the live one. Terminal until a driver
// restart clears the orphan; the live VFS itself keeps working.
func isAmbiguousVFSError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "more than one VFS active")
}

// warnOnDuplicateVFS logs when rclone has more than one VFS registered under
// this volume's fs name (listed as "name:[i]") — a leaked orphan from an
// unclean unmount that makes vfs/* RPCs ambiguous until the driver restarts.
func (mm *MountManager) warnOnDuplicateVFS() {
	result, err := RPCWithTimeout("vfs/list", map[string]interface{}{}, rpcListTimeout)
	if err != nil || result == nil || result.Output == nil {
		return
	}
	names, ok := result.Output["vfses"].([]interface{})
	if !ok {
		return
	}
	prefix := mm.cryptConfigName + ":"
	count := 0
	for _, n := range names {
		if s, ok := n.(string); ok && (s == prefix || strings.HasPrefix(s, prefix+"[")) {
			count++
		}
	}
	if count > 1 {
		klog.Warningf("Volume %s: %d VFS instances registered for %s — leaked orphan from an unclean unmount; "+
			"vfs/* RPCs for this volume are ambiguous until the driver restarts (dirty-cache upload resume is unaffected)",
			mm.volumeID, count, prefix)
	}
}

// durationOrDefault parses a duration string to nanoseconds, falling back to
// defaultNs when empty or invalid.
func durationOrDefault(s string, defaultNs int64) int64 {
	if s == "" {
		return defaultNs
	}
	if ns, err := parseDurationToNs(s); err == nil && ns > 0 {
		return ns
	}
	klog.Warningf("Invalid duration %q, using default", s)
	return defaultNs
}

// parseDurationToNs parses a duration string like "1h" or "5s" to nanoseconds
func parseDurationToNs(s string) (int64, error) {
	// Simple parser for common duration formats
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid duration: %s", s)
	}

	unit := s[len(s)-1]
	value := s[:len(s)-1]

	var num int64
	_, err := fmt.Sscanf(value, "%d", &num)
	if err != nil {
		return 0, err
	}

	switch unit {
	case 's':
		return num * 1e9, nil
	case 'm':
		return num * 60 * 1e9, nil
	case 'h':
		return num * 3600 * 1e9, nil
	case 'd':
		return num * 86400 * 1e9, nil
	default:
		return 0, fmt.Errorf("unknown duration unit: %c", unit)
	}
}

// parseSizeToBytes parses a size string like "10G" or "100M" to bytes
func parseSizeToBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid size: %s", s)
	}

	unit := s[len(s)-1]
	value := s[:len(s)-1]

	var num int64
	_, err := fmt.Sscanf(value, "%d", &num)
	if err != nil {
		return 0, err
	}

	switch unit {
	case 'K', 'k':
		return num * 1024, nil
	case 'M', 'm':
		return num * 1024 * 1024, nil
	case 'G', 'g':
		return num * 1024 * 1024 * 1024, nil
	case 'T', 't':
		return num * 1024 * 1024 * 1024 * 1024, nil
	default:
		return 0, fmt.Errorf("unknown size unit: %c", unit)
	}
}
