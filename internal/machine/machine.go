// Package machine names the machine terma runs on, the same on every network: the relay's
// heartbeat keys a machine by it (staff's Terma users page holds one row per name), a minted
// key carries it, and a sign-in labels its session with it.
package machine

import (
	"os"
	"strings"
	"sync"
)

// Name is the machine's name up to its first dot, "" when the system has none. A Mac answers
// its local host name (Settings > General > Sharing), not os.Hostname: with no HostName set,
// as on most Macs, macOS rebuilds the kernel's hostname from DHCP or reverse DNS whenever the
// network changes, so one laptop beat as studio.local at home and studio.lan at the office,
// two machines to staff. The domain is cut everywhere: it names the network, not the machine.
var Name = sync.OnceValue(func() string {
	name := localName()
	if name == "" {
		name, _ = os.Hostname()
	}
	return short(name)
})

func short(name string) string {
	if first, _, found := strings.Cut(name, "."); found && first != "" {
		return first
	}
	return name
}
