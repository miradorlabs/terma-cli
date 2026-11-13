package relay

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"sync"
)

type connKey struct{}

// connInfo's sender is looked up at the first export, while the socket still exists, so a
// part held for a later claim keeps its sender after the agent exits. client is the
// connection's client end, server the relay's. desktop is whether the sender was the signed
// Terma app when it was found (Options.GovernanceSender), judged once with the pid.
type connInfo struct {
	client  netip.AddrPort
	server  netip.AddrPort
	mu      sync.Mutex
	pid     int
	desktop bool
	tries   int
}

// maxSenderTries retries a lookup that ran before the socket reached the kernel's table.
const maxSenderTries = 5

// ConnContext is the http.Server hook that remembers each connection's two ends.
func (r *Relay) ConnContext(ctx context.Context, c net.Conn) context.Context {
	info := &connInfo{}
	if addr, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		info.client = addr.AddrPort()
	}
	if addr, ok := c.LocalAddr().(*net.TCPAddr); ok {
		info.server = addr.AddrPort()
	}
	return context.WithValue(ctx, connKey{}, info)
}

// senderOf is the process that sent req, 0 when unknown, and whether it is the signed Terma
// app; an unknown sender never widens a claim that names processes (claim.At). Both belong to
// the connection: the app is judged with the process that held the socket when it was found,
// never whatever process has its pid by a later export.
func (r *Relay) senderOf(req *http.Request) (pid int, desktop bool) {
	info, ok := req.Context().Value(connKey{}).(*connInfo)
	if !ok || r.opts.PeerPID == nil || !info.client.IsValid() || !info.server.IsValid() {
		return 0, false
	}
	info.mu.Lock()
	defer info.mu.Unlock()
	if info.pid != 0 || info.tries >= maxSenderTries {
		return info.pid, info.desktop
	}
	info.tries++
	if pid, ok := r.opts.PeerPID(info.client, info.server); ok {
		info.pid = pid
		info.desktop = r.opts.GovernanceSender != nil && r.opts.GovernanceSender(pid)
	} else {
		r.stats.add("sender_unresolved", 1)
	}
	return info.pid, info.desktop
}
