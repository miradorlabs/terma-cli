package machine

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// localName is the name the Mac's user gave it, "" when none can be read.
func localName() string { return firstSet(scutil) }

// firstSet is macOS's own choice of hostname without the network's step: HostName when the
// user set one (the kernel's hostname is then pinned to it), else LocalHostName, the name in
// Settings > General > Sharing, which only a rename changes, or Bonjour when another machine
// on the network already holds it (studio becomes studio-2).
func firstSet(get func(key string) string) string {
	for _, key := range []string{"HostName", "LocalHostName"} {
		if v := get(key); v != "" {
			return v
		}
	}
	return ""
}

// scutil reads key from System Configuration, "" when it is unset (scutil exits 1) or the
// read fails. Release builds leave out cgo, so it runs scutil: about 6 ms.
func scutil(key string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
