// Package machine names the machine terma runs on, the same on every network: the relay's
// heartbeat keys a machine by it (staff's Terma users page holds one row per name), a minted
// key carries it, and a sign-in labels its session with it.
package machine

import (
	"net"
	"os"
	"strings"
	"sync"
)

var (
	mu    sync.Mutex
	local string // localName's answer, once it had one
)

// Name is the machine's name up to its first dot, "" when the system has none. A Mac answers
// the name its user gave it (localName), not os.Hostname: with no HostName set, as on most
// Macs, macOS rebuilds the kernel's hostname from DHCP or reverse DNS whenever the network
// changes, so one laptop beat as studio.local at home and c-73-1-2-3.hsd1.ca.comcast.net on
// another network, two machines to staff. A failed read is tried again at the next call, not
// kept for the life of the relay.
func Name() string {
	mu.Lock()
	defer mu.Unlock()
	if local == "" {
		local = localName()
	}
	return pick(local, os.Hostname)
}

// pick is the system's own name for the machine, else its hostname, cut at the first dot:
// the domain names the network, not the machine.
func pick(local string, hostname func() (string, error)) string {
	name := local
	if name == "" {
		name, _ = hostname()
	}
	return short(name)
}

// short cuts name at its first dot, unless name is an address, which has no domain to cut.
func short(name string) string {
	if net.ParseIP(name) != nil {
		return name
	}
	if first, _, found := strings.Cut(name, "."); found && first != "" {
		return first
	}
	return name
}
