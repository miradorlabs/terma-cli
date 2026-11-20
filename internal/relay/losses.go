package relay

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// lossesFile, in the outbox directory, is what the outbox has dropped: its name starts
// with a dot, so no route is read from it.
const lossesFile = ".losses.json"

// OutboxLosses are the records the outbox dropped because it was full or they were older
// than it keeps, counted since the outbox was made, for doctor to report. Nothing else
// the outbox holds is ever dropped: a refused send, another account's backlog and a
// sign-in that cannot send now all wait.
type OutboxLosses struct {
	Full    int `json:"full,omitempty"`
	Expired int `json:"expired,omitempty"`
	// Held counts claimed records a stopping relay dropped from memory, where they waited
	// for their team's policy: they never reach the disk, so they cannot outlive the relay.
	Held int       `json:"held,omitempty"`
	Last time.Time `json:"last,omitzero"`
}

// ReadOutboxLosses reads what the outbox under dir has dropped; none when it never has.
func ReadOutboxLosses(dir string) (OutboxLosses, error) {
	var l OutboxLosses
	data, err := os.ReadFile(filepath.Join(dir, lossesFile))
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	return l, json.Unmarshal(data, &l)
}

// recordHeldLosses adds held claimed records a stopping relay dropped.
func (o outbox) recordHeldLosses(n int, now time.Time) {
	if o.dir == "" || n == 0 {
		return
	}
	l, _ := ReadOutboxLosses(o.dir)
	l.Held += n
	l.Last = now.UTC()
	_ = config.WriteJSON(filepath.Join(o.dir, lossesFile), l, 0o600)
}

// recordLosses adds full and expired records to the outbox's count.
func (o outbox) recordLosses(full, expired int, now time.Time) {
	if o.dir == "" || full+expired == 0 {
		return
	}
	l, _ := ReadOutboxLosses(o.dir)
	l.Full += full
	l.Expired += expired
	l.Last = now.UTC()
	_ = config.WriteJSON(filepath.Join(o.dir, lossesFile), l, 0o600)
}
