//go:build !unix

package hookrun

import (
	"io/fs"
	"os"
)

// dirHandle wraps os.Root with post-open os.SameFile verification against the lstat result on non-Unix platforms.
type dirHandle struct{ r *os.Root }

func openRootDir(volRoot string) (dirHandle, error) {
	r, err := os.OpenRoot(volRoot)
	return dirHandle{r}, err
}
func (d dirHandle) close()                                 { _ = d.r.Close() }
func (d dirHandle) lstat(name string) (fs.FileInfo, error) { return d.r.Lstat(name) }
func (d dirHandle) readlink(name string) (string, error)   { return d.r.Readlink(name) }
func (d dirHandle) openSubDir(name string, want fs.FileInfo) (dirHandle, error) {
	r, err := d.r.OpenRoot(name)
	if err != nil {
		return dirHandle{}, err
	}
	if got, sErr := r.Stat("."); sErr != nil || !os.SameFile(want, got) {
		_ = r.Close()
		return dirHandle{}, errOpaque
	}
	return dirHandle{r}, nil
}
func (d dirHandle) open(name string, dir bool, want fs.FileInfo) (*os.File, error) {
	f, err := d.r.Open(name)
	if err != nil {
		return nil, err
	}
	if got, sErr := f.Stat(); sErr != nil || !os.SameFile(want, got) || dir && !got.IsDir() || !dir && !got.Mode().IsRegular() {
		_ = f.Close()
		return nil, errOpaque
	}
	return f, nil
}
func sameDevice(_, _ fs.FileInfo) bool { return true }
