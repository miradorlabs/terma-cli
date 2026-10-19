package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// A relay that finds the address setup recorded held by another process does not wait for
// it: it listens on a free port of the same host instead, records that as its address, and
// tells the CLI (Config.Moved) to point the agents' exporters there.

// inUse reports whether listening on addr failed because another process holds it.
func inUse(err error, addr string) bool {
	return errors.Is(err, syscall.EADDRINUSE) || Squatted(addr)
}

// Move is the relay's move off an address another process held (MovedFile).
type Move struct {
	From string    `json:"from"`
	To   string    `json:"to"`
	At   time.Time `json:"at"`
}

// move listens on a free port of the host of from, which another process holds, and records
// it as the relay's address, and the move: on a machine shared with other accounts, what
// holds the port may be one of theirs, and every export to it theirs to read.
func move(dir, from string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(from)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, err
	}
	to := ln.Addr().String()
	if err := config.WriteFileAtomic(filepath.Join(dir, AddrFile), []byte(to+"\n"), 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	if data, err := json.Marshal(Move{From: from, To: to, At: time.Now().UTC()}); err == nil {
		_ = config.WriteFileAtomicNoSync(filepath.Join(dir, MovedFile), append(data, '\n'), 0o600)
	}
	_ = os.Remove(filepath.Join(dir, ErrorFile))
	return ln, nil
}

// MoveNotice is how long after a move doctor and setup still tell of it: agents started
// before it keep sending to the address the relay left until they are restarted.
const MoveNotice = 24 * time.Hour

// RecentMove is the relay's last move off an address another process held, when it made
// one within MoveNotice of now.
func RecentMove(dir string, now time.Time) (Move, bool) {
	data, err := os.ReadFile(filepath.Join(dir, MovedFile))
	if err != nil {
		return Move{}, false
	}
	var m Move
	if json.Unmarshal(data, &m) != nil || m.From == "" || m.To == "" || now.Sub(m.At) >= MoveNotice {
		return Move{}, false
	}
	return m, true
}

// Describe says what the move means for the developer.
func (m Move) Describe() string {
	return fmt.Sprintf("another process listened on %s, so the relay moved to %s; coding agents started before %s still send to %s until restarted",
		m.From, m.To, m.At.Local().Format("Jan 2 15:04"), m.From)
}
