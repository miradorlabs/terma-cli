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

// lossesFile, in the outbox directory, is what the relay has lost. A file, so no route is
// read from it.
const lossesFile = "losses.json"

// lossWindow is how long a loss is counted: as long as the outbox keeps a part. A loss after
// a quiet window starts the counts again, so they say what was lost lately.
const lossWindow = maxOutboxAge

// Why a claimed part is lost as it is placed: it was collected with no sign-in to bind it
// to. Each is also its drop's reason in the stats.
const (
	lostSignedOut  = "signed_out" // a person's profile, signed out
	lostUnreadable = "unreadable" // the profile or its sign-in could not be read, even after a retry
)

// lostPolicyTimeout is the loss reason of claimed records dropped from the hold while they
// waited for their team's policy or key, which the relay couldn't read: past policyHold, for
// room, or as the relay stopped.
const lostPolicyTimeout = "policy_timeout"
const lostNoDestination = "no_destination"

// OutboxLosses are the claimed records the relay lost, counted from the first loss since a
// quiet lossWindow, for doctor to report. Nothing else it queues is ever lost: a refused
// send, another account's backlog and a sign-in that cannot send now all wait.
type OutboxLosses struct {
	// Counts are the records lost by reason: outbox_full and outbox_expired, past the
	// outbox's room or age; policy_timeout, dropped from memory while they waited for a
	// team's policy or key the relay couldn't read; signed_out and unreadable, collected
	// with no sign-in to bind them to.
	Counts map[string]int `json:"counts,omitempty"`
	Last   time.Time      `json:"last,omitzero"`
}

// ReadOutboxLosses reads what the relay with its outbox under dir has lost; none when it
// never has.
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

// recordLosses adds lost, records by reason, to what the relay has lost. Under lossMu, since
// exports, the sweep and the janitor each count.
func (r *Relay) recordLosses(lost map[string]int, now time.Time) {
	if r.outbox.dir == "" {
		return
	}
	r.lossMu.Lock()
	defer r.lossMu.Unlock()
	recordLossesDir(r.outbox.dir, lost, now)
}

func recordLossesDir(dir string, lost map[string]int, now time.Time) {
	var l OutboxLosses
	for reason, n := range lost {
		if n == 0 {
			continue
		}
		if l.Counts == nil {
			if l, _ = ReadOutboxLosses(dir); now.Sub(l.Last) > lossWindow || l.Counts == nil {
				l = OutboxLosses{Counts: map[string]int{}}
			}
		}
		l.Counts[reason] += n
	}
	if l.Counts == nil {
		return
	}
	l.Last = now.UTC()
	_ = config.WriteJSON(filepath.Join(dir, lossesFile), l, 0o600)
}
