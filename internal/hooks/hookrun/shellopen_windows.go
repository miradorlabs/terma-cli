//go:build windows

package hookrun

import (
	"cmp"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// dirHandle wraps os.Root with post-open os.SameFile verification against the lstat result on Windows.
type dirHandle struct{ r *os.Root }

func openRootDir(v string) (dirHandle, error)         { r, e := os.OpenRoot(v); return dirHandle{r}, e }
func (d dirHandle) close()                            { _ = d.r.Close() }
func (d dirHandle) readlink(n string) (string, error) { return d.r.Readlink(n) }
func (d dirHandle) lstat(name string) (fs.FileInfo, error) {
	if strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) }) {
		return nil, fs.ErrNotExist
	}
	fi, err := d.r.Lstat(name)
	return fi, pick(errors.Is(err, syscall.Errno(123)) || errors.Is(err, syscall.Errno(161)) || errors.Is(err, syscall.Errno(206)), fs.ErrNotExist, err)
}
func (d dirHandle) openSubDir(name string, want fs.FileInfo) (dirHandle, error) {
	r, err := d.r.OpenRoot(name)
	if err != nil {
		return dirHandle{}, err
	} else if got, sErr := r.Stat("."); sErr == nil && os.SameFile(want, got) {
		return dirHandle{r}, nil
	}
	_ = r.Close()
	return dirHandle{}, errOpaque
}
func (d dirHandle) open(name string, dir bool, want fs.FileInfo) (*os.File, error) {
	f, err := d.r.Open(name)
	if err != nil {
		return nil, err
	} else if got, sErr := f.Stat(); sErr == nil && os.SameFile(want, got) && (dir && got.IsDir() || !dir && got.Mode().IsRegular()) {
		return f, nil
	}
	_ = f.Close()
	return nil, errOpaque
}
func sameDevice(_, _ fs.FileInfo) bool { return true }
func fileOwner(p string) (*windows.SID, bool) {
	if sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION); err == nil && sd != nil {
		owner, _, err := sd.Owner()
		return owner, err == nil && owner != nil
	}
	return nil, false
}
func isSysBin(p string, _ fs.FileInfo) bool {
	owner, ok := fileOwner(p)
	return ok && strings.EqualFold(filepath.VolumeName(p), cmp.Or(os.Getenv("SystemDrive"), "C:")) && (owner.IsWellKnown(windows.WinLocalSystemSid) || owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) || owner.String() == "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464")
}
func sameOwner(p string, _ fs.FileInfo) bool {
	owner, ok := fileOwner(p)
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	return ok && err == nil && (owner.Equals(tu.User.Sid) || owner.IsWellKnown(windows.WinBuiltinAdministratorsSid) && windows.GetCurrentProcessToken().IsElevated())
}
