package daemon

import (
	"context"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// A stopping relay closes its listener before it shuts its server down, and lets what it
// already accepted reach its handler first: net/http drops, unanswered, a request it reads
// once Shutdown has begun, and the agent never resends it.

// newRequestWait bounds how long a stopping relay waits for accepted connections to send
// their request: an exporter writes it as soon as it connects.
const newRequestWait = time.Second

type connKey struct{}

// onFreshNew, set only in tests, observes each StateNew connection as fresh tracks it.
var onFreshNew atomic.Pointer[func(net.Conn)]

// fresh counts accepted connections that have not yet entered their first handler.
// net/http fires StateActive as soon as the first byte is read, before checking
// Server.shuttingDown; removing on StateActive lets Shutdown race readRequest and drop
// the connection unanswered.
type fresh struct {
	mu    sync.Mutex
	conns map[net.Conn]bool
}

func (f *fresh) track(c net.Conn, s http.ConnState) {
	f.mu.Lock()
	if f.conns == nil {
		f.conns = map[net.Conn]bool{}
	}
	switch s {
	case http.StateNew:
		f.conns[c] = true
	case http.StateIdle, http.StateClosed, http.StateHijacked:
		delete(f.conns, c)
	}
	f.mu.Unlock()
	if s == http.StateNew {
		if fn := onFreshNew.Load(); fn != nil {
			(*fn)(c)
		}
	}
}

func (f *fresh) connContext(next func(context.Context, net.Conn) context.Context) func(context.Context, net.Conn) context.Context {
	return func(ctx context.Context, c net.Conn) context.Context {
		if next != nil {
			ctx = next(ctx, c)
		}
		return context.WithValue(ctx, connKey{}, c)
	}
}

func (f *fresh) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, ok := r.Context().Value(connKey{}).(net.Conn); ok {
			f.mu.Lock()
			delete(f.conns, c)
			f.mu.Unlock()
		}
		next.ServeHTTP(w, r)
	})
}

// await waits up to wait for every fresh connection to reach the handler or close.
func (f *fresh) await(wait time.Duration) {
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		f.mu.Lock()
		n := len(f.conns)
		f.mu.Unlock()
		if n == 0 {
			return
		}
	}
}
