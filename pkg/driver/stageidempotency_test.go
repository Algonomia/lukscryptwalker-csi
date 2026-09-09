package driver

import (
	"testing"

	"github.com/lukscryptwalker-csi/pkg/luks"
)

// A re-stage must never mistake a plain directory for a staged volume: kubelet
// re-issues NodeStageVolume for every volume it holds after a restart, and both
// wrong answers are damaging. Saying "staged" binds consumers onto the empty
// directory under the mount; saying "not staged" over a live mount tears it out
// and leaks the VFS behind it.
func TestIsAlreadyStagedRoutesOnBackend(t *testing.T) {
	ns := &NodeServer{luksManager: luks.NewLUKSManager()}
	s3 := map[string]string{StorageBackendParam: "s3"}

	// An S3 volume must be judged by its FUSE mount, not by a LUKS mapper it
	// will never have — the bug this guards is the LUKS check answering for S3
	// and reporting every re-stage as unstaged.
	staged, err := ns.isAlreadyStaged("vol-s3", t.TempDir(), s3)
	if err != nil {
		t.Fatalf("a readable mount table must yield a verdict, got error: %v", err)
	}
	if staged {
		t.Error("a plain directory is not a staged S3 volume")
	}

	// No storage-backend key at all is the LUKS path, which needs its mapper.
	if staged, err := ns.isAlreadyStaged("vol-luks", t.TempDir(), nil); err != nil || staged {
		t.Errorf("a LUKS volume with no open mapper is not staged (staged=%v, err=%v)", staged, err)
	}
}
