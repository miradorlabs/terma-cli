//go:build linux

package delivery

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// fileIdentity returns a stable identifier for the current OS file instance at path.
// O_APPEND writes by hooks preserve this identity, while spool.ackBatch's atomic rename
// creates a fresh file instance. ext4 and XFS hand a freed inode number to the next file
// they create, so the queue that replaces an acknowledged one can carry an earlier
// queue's inode: the file's birth time tells the two apart, as the creation time does on
// Windows. A filesystem that keeps no birth time falls back to device and inode.
func fileIdentity(path string) string {
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_INO|unix.STATX_BTIME, &st); err != nil {
		return ""
	}
	id := fmt.Sprintf("%x-%x", unix.Mkdev(st.Dev_major, st.Dev_minor), st.Ino)
	if st.Mask&unix.STATX_BTIME != 0 {
		id += fmt.Sprintf("-%x-%x", st.Btime.Sec, st.Btime.Nsec)
	}
	return id
}
