package daemon

import (
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// A stopping relay stops accepting before it shuts its server down, and lets what it
// already accepted send its request first: net/http drops, unanswered, a request it reads
// once Shutdown has begun, and the agent never resends it. What it did not accept waits in
// the socket for the next relay.

var errStoppedAccepting = errors.New("relay: stopped accepting")

// newRequestWait bounds how long a stopping relay waits for accepted connections to send
// their request: an exporter writes it as soon as it connects.
const newRequestWait = time.Second

// gate is the relay's listener, which can stop accepting without closing the socket.
type gate struct {
	net.Listener
	stopped atomic.Bool
}

// Accept returns a connection accepted even as the gate stops, which is then served.
func (g *gate) Accept() (net.Conn, error) {
	c, err := g.Listener.Accept()
	if err != nil && g.stopped.Load() {
		return nil, errStoppedAccepting
	}
	return c, err
}

// stop wakes a pending Accept, by a deadline on this descriptor only: a duplicate handed
// to the next relay accepts as before.
func (g *gate) stop() {
	g.stopped.Store(true)
	if d, ok := g.Listener.(interface{ SetDeadline(time.Time) error }); ok {
		_ = d.SetDeadline(time.Now())
	} else {
		_ = g.Close()
	}
}

// fresh counts the connections that have not yet sent a request (http.StateNew).
type fresh struct {
	mu    sync.Mutex
	conns map[net.Conn]bool
}

func (f *fresh) track(c net.Conn, s http.ConnState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conns == nil {
		f.conns = map[net.Conn]bool{}
	}
	if s == http.StateNew {
		f.conns[c] = true
	} else {
		delete(f.conns, c)
	}
}

// await waits up to wait for every fresh connection to send its request.
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
