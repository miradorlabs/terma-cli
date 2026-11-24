//go:build !unix

package hookrun

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

// dirHandle wraps os.Root with post-open os.SameFile verification against the lstat result on non-Unix platforms.
type dirHandle struct{ r *os.Root }

func openRootDir(volRoot string) (dirHandle, error) {
	r, err := os.OpenRoot(volRoot)
	return dirHandle{r}, err
}
func (d dirHandle) close()                            { _ = d.r.Close() }
func (d dirHandle) readlink(n string) (string, error) { return d.r.Readlink(n) }
func (d dirHandle) lstat(name string) (fs.FileInfo, error) {
	if strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) }) {
		return nil, fs.ErrNotExist
	}
	fi, err := d.r.Lstat(name)
	return fi, pick(errors.Is(err, syscall.Errno(123)) || errors.Is(err, syscall.Errno(161)), fs.ErrNotExist, err)
}
func (d dirHandle) openSubDir(name string, want fs.FileInfo) (dirHandle, error) {
	if r, err := d.r.OpenRoot(name); err != nil {
		return dirHandle{}, err
	} else if got, sErr := r.Stat("."); sErr == nil && os.SameFile(want, got) {
		return dirHandle{r}, nil
	} else {
		_ = r.Close()
		return dirHandle{}, errOpaque
	}
}
func (d dirHandle) open(name string, dir bool, want fs.FileInfo) (*os.File, error) {
	if f, err := d.r.Open(name); err != nil {
		return nil, err
	} else if got, sErr := f.Stat(); sErr == nil && os.SameFile(want, got) && (dir && got.IsDir() || !dir && got.Mode().IsRegular()) {
		return f, nil
	} else {
		_ = f.Close()
		return nil, errOpaque
	}
}
func sameDevice(_, _ fs.FileInfo) bool { return true }
