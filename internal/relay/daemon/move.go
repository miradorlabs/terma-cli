package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// A relay that cannot listen on the address setup recorded does not keep failing there:
// another process may hold it, and on a machine shared with other accounts it may be one of
// theirs, which every export to it would reach. It listens on a free port of the same host
// instead, records that as its address and the move, and tells the CLI (Config.Moved) to
// point the agents' exporters there.

// Move is the relay's last move off an address it could not listen on (MovedFile).
type Move struct {
	From string    `json:"from"`
	To   string    `json:"to"`
	At   time.Time `json:"at"`
	// Repointed means `terma setup` has since pointed every agent it set up at To, those
	// whose exporter the relay cannot read included.
	Repointed bool `json:"repointed,omitempty"`
}

// Ports a moved relay picks from: below every platform's default ephemeral range (Linux's
// starts at 32768, macOS's and Windows' at 49152), so no client's port is ever its own.
const (
	movePortMin   = 20000
	movePortMax   = 32767
	movePortTries = 20
)

// move listens on a free port of the host of from, where the relay could not, and records
// it as the relay's address, and the move. It moves nothing when the recorded address is no
// longer from: setup has just chosen another.
func move(dir, from string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(from)
	if err != nil {
		return nil, err
	}
	if Addr(dir) != from {
		return nil, errors.New("relay: the recorded address changed")
	}
	ln, err := listenFree(host)
	if err != nil {
		return nil, err
	}
	to := ln.Addr().String()
	if err := config.WriteFileAtomic(filepath.Join(dir, AddrFile), []byte(to+"\n"), 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	writeMove(dir, Move{From: from, To: to, At: time.Now().UTC()})
	_ = os.Remove(filepath.Join(dir, ErrorFile))
	return ln, nil
}

// listenFree listens on a random port of host in the move range, else any port.
func listenFree(host string) (net.Listener, error) {
	for range movePortTries {
		port := movePortMin + rand.IntN(movePortMax-movePortMin+1)
		if ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port))); err == nil {
			return ln, nil
		}
	}
	return net.Listen("tcp", net.JoinHostPort(host, "0"))
}

func writeMove(dir string, m Move) {
	if data, err := json.Marshal(m); err == nil {
		_ = config.WriteFileAtomicNoSync(filepath.Join(dir, MovedFile), append(data, '\n'), 0o600)
	}
}

// LastMove is the relay's last move off an address it could not listen on, if it made one.
func LastMove(dir string) (Move, bool) {
	data, err := os.ReadFile(filepath.Join(dir, MovedFile))
	if err != nil {
		return Move{}, false
	}
	var m Move
	if json.Unmarshal(data, &m) != nil || m.From == "" || m.To == "" {
		return Move{}, false
	}
	return m, true
}

// Repointed records that `terma setup` has pointed every agent at where the relay moved.
func Repointed(dir string) {
	if m, ok := LastMove(dir); ok && !m.Repointed {
		m.Repointed = true
		writeMove(dir, m)
	}
}

// MoveNotice is how long after a move doctor still tells of it: agents started before it
// keep sending to the address the relay left until they are restarted.
const MoveNotice = 24 * time.Hour

// Recent reports whether the move is within MoveNotice of now.
func (m Move) Recent(now time.Time) bool { return now.Sub(m.At) < MoveNotice }

// Describe says what happened, and what it means for agents already running.
func (m Move) Describe() string {
	return fmt.Sprintf("the relay could not listen on %s, which another process may hold, so it moved to %s at %s; coding agents started before then still send to %s until restarted",
		m.From, m.To, m.At.Local().Format("Jan 2 15:04"), m.From)
}
