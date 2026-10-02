package daemon

import (
	"net"
	"os"
	"path/filepath"
	"time"
)

// A relay that stops while its setup remains hands its listening socket to the relay that
// takes the lock after it, so the port is never unbound: an agent never resends what it
// exported while nothing listened. Between the stopping relay's last accept and its
// successor's first, connections wait in the socket's backlog instead of being refused.
// Windows cannot pass a socket this way (handoffSupported), and keeps the gap.

// WaitingFile is touched by the service's relay while another relay holds the lock, so
// an on-demand relay steps aside for it.
const WaitingFile = "service-waiting"

const (
	// handoffWait bounds how long a stopping relay keeps its socket for a successor, which
	// takes it within a lock poll: with the drain, a stop stays under launchd's 20 s
	// ExitTimeOut before it kills the relay.
	handoffWait = 5 * time.Second
	// successorWait bounds how long a successor waits for the stopping relay to drain.
	successorWait = 30 * time.Second
	successorPoll = 20 * time.Millisecond
	// waitingFresh is how recently a waiting service relay touched WaitingFile.
	waitingFresh = 2 * time.Second
)

// serviceWaiting reports whether the service's relay waits for the lock.
func serviceWaiting(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, WaitingFile))
	return err == nil && time.Since(info.ModTime()) < waitingFresh
}

func touchWaiting(dir string) {
	path := filepath.Join(dir, WaitingFile)
	now := time.Now()
	if os.Chtimes(path, now, now) != nil {
		_ = os.WriteFile(path, nil, 0o600)
	}
}

// handoff is a stopping relay's socket, kept open past its listener's close for the next relay.
type handoff struct {
	dir, addr string
	sock      *os.File
	spawned   bool
}

// startHandoff keeps ln's socket for the relay after this one, and starts that relay with
// spawn unless the service's relay already waits for it: started now, it is ready by the
// time this one has drained. Nil when there is nothing to hand.
func startHandoff(dir, addr string, ln net.Listener, spawn func() error) *handoff {
	if !handoffSupported {
		return nil
	}
	sock, err := listenerFile(ln)
	if err != nil {
		return nil
	}
	h := &handoff{dir: dir, addr: addr, sock: sock}
	if !serviceWaiting(dir) && spawn != nil {
		h.spawned = spawn() == nil
	}
	return h
}

// offer is called while the lock is still held, so the successor finds the offer as soon
// as it takes the lock; the returned func, called once the lock is released, waits for it
// and reports whether it took the socket. Nil when no successor is coming.
func (h *handoff) offer() func() bool {
	if h == nil {
		return nil
	}
	if !h.spawned && !serviceWaiting(h.dir) {
		_ = h.sock.Close()
		return nil
	}
	await, err := offerListener(h.dir, h.addr, h.sock)
	if err != nil {
		_ = h.sock.Close()
		return nil
	}
	return func() bool {
		defer func() { _ = h.sock.Close() }()
		return await(handoffWait)
	}
}
