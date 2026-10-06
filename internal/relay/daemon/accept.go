package daemon

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// A stopping relay closes its listener before it shuts its server down, and lets what it
// already accepted send its request first: net/http drops, unanswered, a request it reads
// once Shutdown has begun, and the agent never resends it.

// newRequestWait bounds how long a stopping relay waits for accepted connections to send
// their request: an exporter writes it as soon as it connects.
const newRequestWait = time.Second

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
