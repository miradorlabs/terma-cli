//go:build !unix

package hookrun

import (
	"errors"
	"io/fs"
	"os"
)

func openDirNoBlock(dir string) (*os.File, error) {
	if info, err := os.Stat(dir); err != nil {
		return nil, err
	} else if !info.IsDir() {
		return nil, fs.ErrInvalid
	}
	return os.Open(dir)
}
func openFileNoBlock(p string) (*os.File, error) { return os.Open(p) }
func isBenignDirErr(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrInvalid)
}
