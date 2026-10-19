package daemon

import (
	"context"
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
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// A relay that cannot listen on the address setup recorded does not keep failing there:
// another process may hold it, and on a machine shared with other accounts it may be one of
// theirs, which every export to it would reach. It listens on a free port of the same host
// instead, records that as its address and the move, and tells the CLI (Config.Moved) to
// point the agents' exporters there.

// listen writes a failure where status reads it, since a hook-started relay has nowhere to print.
func listen(dir, addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		err = fmt.Errorf("relay: listen on %s: %w", addr, err)
		_ = config.WriteFileAtomicNoSync(filepath.Join(dir, ErrorFile), []byte(time.Now().UTC().Format(time.RFC3339)+" "+err.Error()+"\n"), 0o600)
		return nil, err
	}
	_ = os.Remove(filepath.Join(dir, ErrorFile))
	return ln, nil
}

// Move is the relay's last move off an address it could not listen on (MovedFile).
type Move struct {
	From string    `json:"from"`
	To   string    `json:"to"`
	At   time.Time `json:"at"`
	// Repointed means `terma setup` has since pointed every agent it set up at To, those
	// whose exporter the relay cannot read included.
	Repointed bool `json:"repointed,omitempty"`
	// Replaced are the relay tokens moves replaced, latest last: what held the address may
	// have read one from an export (rotateToken).
	Replaced []string `json:"replaced,omitempty"`
}

// Ports a moved relay picks from: below every platform's default ephemeral range (Linux's
// starts at 32768, macOS's and Windows' at 49152), so no client's port is ever its own.
const (
	movePortMin   = 20000
	movePortMax   = 32767
	movePortTries = 20
)

// addrLockFile is held by RecordAddr and move while they write the relay's address.
const addrLockFile = "addr.lock"

// addrLockWait bounds the wait for addrLockFile; a variable so a test need not wait it out.
var addrLockWait = 5 * time.Second

// RecordAddr records addr as where the relay listens, never in the middle of a relay's move.
// A lock that cannot be had costs the write its exclusivity, never the write: the address
// is the developer's choice.
func RecordAddr(dir, addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), addrLockWait)
	unlock, err := flock.Lock(ctx, filepath.Join(dir, addrLockFile))
	cancel()
	if err == nil {
		defer unlock()
	}
	return config.WriteFileAtomic(filepath.Join(dir, AddrFile), []byte(addr+"\n"), 0o600)
}

// move listens on a free port of the host of from, where the relay could not, and records
// it as the relay's address, and the move. It moves nothing when the recorded address is no
// longer from: setup has just chosen another (RecordAddr).
func move(dir, from string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(from)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), addrLockWait)
	unlock, err := flock.Lock(ctx, filepath.Join(dir, addrLockFile))
	cancel()
	if err != nil {
		return nil, err
	}
	defer unlock()
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
	prev, _ := LastMove(dir)
	writeMove(dir, Move{From: from, To: to, At: time.Now().UTC(), Replaced: prev.Replaced})
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

// Repointed records that `terma setup` has pointed every agent at to, where the relay moved
// last; a later move is left as it is.
func Repointed(dir, to string) {
	if m, ok := LastMove(dir); ok && m.To == to && !m.Repointed {
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
