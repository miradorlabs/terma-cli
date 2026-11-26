//go:build unix && !linux

package delivery

import (
	"fmt"
	"os"
	"syscall"
)

// fileIdentity returns a stable identifier for the current OS file instance at path.
// O_APPEND writes by hooks preserve this identity, while spool.ackBatch's atomic rename
// creates a fresh file instance (paired with the batch's first-event hash by callers so
// eventual Unix inode reuse does not collide with an earlier distinct batch).
func fileIdentity(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%x-%x", st.Dev, st.Ino)
}
