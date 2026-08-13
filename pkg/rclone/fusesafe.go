package rclone

import (
	"syscall"
	"unsafe"
)

// Raw-syscall filesystem access for paths that may be a wedged FUSE mount.
// os.Open/os.ReadDir hand the descriptor to Go's netpoller, whose epoll_ctl
// calls the FUSE ->poll handler and never returns if the server is dead —
// wedging the whole runtime, not just the caller. A timeout does not help; only
// keeping the descriptor away from netpoll does. os.NewFile (hence RemoveAll)
// is safe: blocking descriptors are not registered.

// DirEntryRaw is a directory entry read without constructing an os.File.
type DirEntryRaw struct {
	Name  string
	IsDir bool
	// IsRegular is false for DT_UNKNOWN entries; probe those anyway.
	IsRegular bool
	Unknown   bool
}

// ReadDirRaw lists up to max entries using open/getdents/close directly.
func ReadDirRaw(path string, max int) ([]DirEntryRaw, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = syscall.Close(fd) }()

	buf := make([]byte, 8192)
	var out []DirEntryRaw
	for len(out) < max {
		n, err := syscall.ReadDirent(fd, buf)
		if err != nil {
			return out, err
		}
		if n <= 0 {
			break
		}
		for off := 0; off < n; {
			d := (*syscall.Dirent)(unsafe.Pointer(&buf[off]))
			if d.Reclen == 0 || off+int(d.Reclen) > n {
				break
			}
			off += int(d.Reclen)

			name := direntName(d)
			if name == "" || name == "." || name == ".." {
				continue
			}
			out = append(out, DirEntryRaw{
				Name:      name,
				IsDir:     d.Type == syscall.DT_DIR,
				IsRegular: d.Type == syscall.DT_REG,
				Unknown:   d.Type == syscall.DT_UNKNOWN,
			})
			if len(out) >= max {
				break
			}
		}
	}
	return out, nil
}

// direntName extracts the NUL-terminated name from a dirent.
func direntName(d *syscall.Dirent) string {
	raw := (*[256]byte)(unsafe.Pointer(&d.Name[0]))[:]
	for i, c := range raw {
		if c == 0 {
			return string(raw[:i])
		}
	}
	return ""
}

// OpenProbeRaw opens and closes a path to prove the filesystem still serves
// real opens: a cached listing succeeds even when a cancelled VFS fails them.
func OpenProbeRaw(path string) error {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	return syscall.Close(fd)
}
