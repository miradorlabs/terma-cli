//go:build unix

package harness

import (
	"fmt"
	"os"
	"syscall"
)

func rolloutFileIdentity(st os.FileInfo) string {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d:", s.Dev, s.Ino)
	}
	return ""
}
