package rclone

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"k8s.io/klog"
)

// cacheItemInfo mirrors the fields of rclone's vfscache metadata we validate.
type cacheItemInfo struct {
	Size  int64 `json:"Size"`
	Dirty bool  `json:"Dirty"`
	Rs    []struct {
		Pos  int64 `json:"Pos"`
		Size int64 `json:"Size"`
	} `json:"Rs"`
}

// ValidateVFSCache scans a volume's cache before mounting and removes or
// quarantines items whose metadata is inconsistent with the data file (ranges
// past EOF, unparseable meta). rclone can never reload such items — consumers
// block on them indefinitely ("invalid seek position"), stalling whole nodes.
func ValidateVFSCache(vfsName string) {
	validateVFSCacheAt(VFSCacheBasePath, vfsName)
}

func validateVFSCacheAt(base, vfsName string) {
	metaRoot := filepath.Join(base, "vfsMeta", vfsName)
	dataRoot := filepath.Join(base, "vfs", vfsName)

	_ = filepath.WalkDir(metaRoot, func(metaPath string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(metaRoot, metaPath)
		if err != nil {
			return nil
		}
		dataPath := filepath.Join(dataRoot, rel)

		reason := corruptReason(metaPath, dataPath)
		if reason == "" {
			return nil
		}

		if isCacheItemDirty(metaPath) {
			quarantineCacheItem(base, vfsName, rel, metaPath, dataPath, reason)
			return nil
		}
		klog.Warningf("VFS cache item %s/%s is corrupt (%s) and not dirty; removing so it re-downloads from S3", vfsName, rel, reason)
		_ = os.Remove(metaPath)
		_ = os.Remove(dataPath)
		return nil
	})
}

// corruptReason reports why an item cannot be reloaded, or "" when healthy.
func corruptReason(metaPath, dataPath string) string {
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return "unreadable metadata"
	}
	var info cacheItemInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return "unparseable metadata"
	}

	var dataSize int64
	if st, err := os.Stat(dataPath); err == nil {
		dataSize = st.Size()
	} else if len(info.Rs) > 0 {
		return "metadata claims cached ranges but data file is missing"
	}
	for _, r := range info.Rs {
		if r.Pos+r.Size > dataSize {
			return fmt.Sprintf("cached range [%d,%d) past data file size %d", r.Pos, r.Pos+r.Size, dataSize)
		}
	}
	return ""
}

func isCacheItemDirty(metaPath string) bool {
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return false
	}
	var info cacheItemInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return false
	}
	return info.Dirty
}

// quarantineCacheItem moves a corrupt-but-dirty item aside: its local bytes
// are the only copy of unuploaded writes, so keep them for manual recovery
// instead of serving an item rclone can never load.
func quarantineCacheItem(base, vfsName, rel, metaPath, dataPath, reason string) {
	qdir := filepath.Join(base, "quarantine", vfsName, filepath.Dir(rel))
	if err := os.MkdirAll(qdir, 0700); err != nil {
		klog.Errorf("VFS cache item %s/%s is corrupt (%s) and DIRTY, but quarantine dir failed: %v — leaving in place", vfsName, rel, reason, err)
		return
	}
	stamp := time.Now().Unix()
	name := filepath.Base(rel)
	_ = os.Rename(dataPath, filepath.Join(qdir, fmt.Sprintf("%s.%d.data", name, stamp)))
	_ = os.Rename(metaPath, filepath.Join(qdir, fmt.Sprintf("%s.%d.meta", name, stamp)))
	klog.Warningf("VFS cache item %s/%s is corrupt (%s) and DIRTY: quarantined to %s — "+
		"the volume serves the last-uploaded S3 version; recover unuploaded bytes manually if needed", vfsName, rel, reason, qdir)
}
