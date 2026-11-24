//go:build !unix

package hookrun

import (
	"io/fs"
	"os"
)

// dirHandle wraps os.Root for handle-relative traversal and opens on non-Unix platforms.
type dirHandle struct{ r *os.Root }

func openRootDir(volRoot string) (dirHandle, error) {
	r, err := os.OpenRoot(volRoot)
	return dirHandle{r}, err
}
func (d dirHandle) close()                                 { _ = d.r.Close() }
func (d dirHandle) lstat(name string) (fs.FileInfo, error) { return d.r.Lstat(name) }
func (d dirHandle) readlink(name string) (string, error)   { return d.r.Readlink(name) }
func (d dirHandle) openSubDir(name string) (dirHandle, error) {
	r, err := d.r.OpenRoot(name)
	return dirHandle{r}, err
}
func (d dirHandle) open(name string, dir bool) (*os.File, error) {
	if !dir || name == "." {
		return d.r.Open(name)
	}
	sub, err := d.r.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	defer sub.Close()
	return sub.Open(".")
}
func sameDevice(_, _ fs.FileInfo) bool { return true }
