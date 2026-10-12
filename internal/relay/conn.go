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
// connection's client end, server the relay's.
type connInfo struct {
	client netip.AddrPort
	server netip.AddrPort
	mu     sync.Mutex
	pid    int
	tries  int
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

// senderPID is the process that sent req, 0 when unknown; an unknown sender never widens a
// claim that names processes (claim.At).
func (r *Relay) senderPID(req *http.Request) int {
	info, ok := req.Context().Value(connKey{}).(*connInfo)
	if !ok || r.opts.PeerPID == nil || !info.client.IsValid() || !info.server.IsValid() {
		return 0
	}
	info.mu.Lock()
	defer info.mu.Unlock()
	if info.pid != 0 || info.tries >= maxSenderTries {
		return info.pid
	}
	info.tries++
	if pid, ok := r.opts.PeerPID(info.client, info.server); ok {
		info.pid = pid
	} else {
		r.stats.add("sender_unresolved", 1)
	}
	return info.pid
}
