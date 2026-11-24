//go:build unix

package hookrun

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// dirHandle wraps a directory FD for TOCTOU-free, non-blocking handle-relative traversal.
type dirHandle int

func openRootDir(p string) (dirHandle, error) { return openAt(unix.AT_FDCWD, p, unix.O_DIRECTORY, nil) }
func (d dirHandle) close()                    { _ = unix.Close(int(d)) }
func (d dirHandle) openSubDir(n string, w fs.FileInfo) (dirHandle, error) {
	return openAt(int(d), n, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK, w)
}
func (d dirHandle) open(name string, dir bool, want fs.FileInfo) (*os.File, error) {
	fd, err := openAt(int(d), name, unix.O_NOFOLLOW|unix.O_NONBLOCK|pick(dir, unix.O_DIRECTORY, 0), want)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
func (d dirHandle) lstat(name string) (fs.FileInfo, error) {
	var st unix.Stat_t
	err := unix.Fstatat(int(d), name, &st, unix.AT_SYMLINK_NOFOLLOW)
	return pick(err == nil, fs.FileInfo(&statInfo{st}), nil), err
}
func (d dirHandle) readlink(name string) (string, error) {
	var buf [maxPathBytes]byte
	if n, err := unix.Readlinkat(int(d), name, buf[:]); err == nil && n < len(buf) {
		return string(buf[:n]), nil
	}
	return "", errOpaque
}
func openAt(dirfd int, name string, flags int, want fs.FileInfo) (dirHandle, error) {
	for {
		fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_CLOEXEC|flags, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		} else if err != nil || want == nil {
			return dirHandle(fd), err
		}
		var st unix.Stat_t
		if ws, ok := want.Sys().(*unix.Stat_t); !ok || unix.Fstat(fd, &st) != nil || st.Dev != ws.Dev || st.Ino != ws.Ino || uint32(st.Mode&unix.S_IFMT) != pick(flags&unix.O_DIRECTORY != 0, uint32(unix.S_IFDIR), uint32(unix.S_IFREG)) {
			_ = unix.Close(fd)
			return -1, errOpaque
		}
		return dirHandle(fd), nil
	}
}

var effectiveUID, modeBits = os.Geteuid, map[uint32]fs.FileMode{unix.S_IFDIR: fs.ModeDir, unix.S_IFLNK: fs.ModeSymlink, unix.S_IFIFO: fs.ModeNamedPipe, unix.S_IFREG: 0}

func statIDs(a fs.FileInfo) (uint64, uint32, bool) {
	if sa, ok := a.Sys().(*unix.Stat_t); ok {
		return uint64(sa.Dev), sa.Uid, true
	} else if sa, ok := a.Sys().(*syscall.Stat_t); ok {
		return uint64(sa.Dev), sa.Uid, true
	}
	return 0, 0, false
}
func sameDevice(a, b fs.FileInfo) bool {
	d1, _, o1 := statIDs(a)
	d2, _, o2 := statIDs(b)
	return o1 && o2 && d1 == d2
}
func sameOwner(_ string, a fs.FileInfo) bool {
	_, u, ok := statIDs(a)
	return ok && u == uint32(effectiveUID())
}
func isSysBin(_ string, a fs.FileInfo) bool {
	_, u, ok := statIDs(a)
	return ok && u == 0 && a.Mode()&0o022 == 0
}

type statInfo struct{ st unix.Stat_t }

func (s *statInfo) Name() string       { return "" }
func (s *statInfo) Size() int64        { return s.st.Size }
func (s *statInfo) ModTime() time.Time { return time.Time{} }
func (s *statInfo) IsDir() bool        { return s.st.Mode&unix.S_IFMT == unix.S_IFDIR }
func (s *statInfo) Sys() any           { return &s.st }
func (s *statInfo) Mode() fs.FileMode {
	bit, ok := modeBits[uint32(s.st.Mode&unix.S_IFMT)]
	return pick(ok, bit, fs.ModeIrregular) | fs.FileMode(s.st.Mode&0o777)
}
