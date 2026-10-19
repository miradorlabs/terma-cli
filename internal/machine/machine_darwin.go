package machine

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// localName is the Mac's LocalHostName, which only a rename changes, or "" when it is unset
// or cannot be read. Release builds leave out cgo, so System Configuration is asked through
// scutil: about 6 ms, once a process.
func localName() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--get", "LocalHostName").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
