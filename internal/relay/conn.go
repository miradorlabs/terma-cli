package relay

import (
	"context"
	"net"
	"net/http"
	"sync"
)

// Senders. A claim covers only the processes whose hooks made it, so the relay needs
// to know which process sent each record. It looks when the connection's first export
// arrives — while the sender's socket still exists, so a record held for a claim that
// lands later keeps its sender after the agent exits — and again at the next few if
// that failed.

type connKey struct{}

// connInfo is one exporter connection: its remote port, and the process behind it,
// looked up at its first export and kept once found.
type connInfo struct {
	port  int
	mu    sync.Mutex
	pid   int
	tries int
}

// maxSenderTries bounds how often one connection's sender is looked up: a failed lookup
// (the socket not yet in the kernel's table) is tried again at the connection's next
// exports, not taken for the connection's life.
const maxSenderTries = 5

// ConnContext is the http.Server hook that remembers each connection's remote port.
func (r *Relay) ConnContext(ctx context.Context, c net.Conn) context.Context {
	info := &connInfo{}
	if addr, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		info.port = addr.Port
	}
	return context.WithValue(ctx, connKey{}, info)
}

// senderPID is the process that sent req, 0 when it cannot be told (no PeerPID, a
// server without ConnContext, a lookup that failed — counted as sender_unresolved).
// An unknown sender is covered only by a claim that names no processes (claim.At): it
// never widens a claim that does.
func (r *Relay) senderPID(req *http.Request) int {
	info, ok := req.Context().Value(connKey{}).(*connInfo)
	if !ok || r.opts.PeerPID == nil || info.port == 0 {
		return 0
	}
	info.mu.Lock()
	defer info.mu.Unlock()
	if info.pid != 0 || info.tries >= maxSenderTries {
		return info.pid
	}
	info.tries++
	if pid, ok := r.opts.PeerPID(info.port); ok {
		info.pid = pid
	} else {
		r.stats.add("sender_unresolved", 1)
	}
	return info.pid
}
