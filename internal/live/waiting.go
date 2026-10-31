package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
)

// AnswerWait is how long a session waits for the developer before they are nudged.
const AnswerWait = 2 * time.Minute

// waitLockWait bounds a hook's wait for the record a check holds.
const waitLockWait = time.Second

// Wait is a session waiting for the developer's answer or permission, from the hook that saw
// it begin until the developer's next sign of life ends it (EndWait).
type Wait struct {
	// ID tells this wait from the session's next, so a check for one never nudges about another.
	ID    string    `json:"id"`
	Since time.Time `json:"since"`
	// Agent is the agent as prose names it, and Checkout the folder it works in.
	Agent    string `json:"agent"`
	Checkout string `json:"checkout,omitempty"`
	// Nudged is set once the developer has been told.
	Nudged bool `json:"nudged,omitempty"`
}

// errNoNudge is a wait that is over, another, already told, or not yet due.
var errNoNudge = errors.New("no nudge due")

// StartWait records w at path, in place of any wait the session had.
func StartWait(path string, w Wait) error {
	data, err := json.Marshal(w)
	if err != nil {
		return err
	}
	return flock.Locked(path, waitLockWait, func() error { return config.WriteFileAtomicNoSync(path, data, 0o600) })
}

// EndWait ends the wait recorded at path, if there is one: the developer is back.
func EndWait(path string) {
	if _, err := os.Lstat(path); err != nil {
		return // nothing waits, the common case: no lock taken
	}
	_ = flock.Locked(path, waitLockWait, func() error {
		if err := config.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
}

// AwaitAnswer sleeps until wait id at path has gone on for AnswerWait, then nudges the
// developer through n if it still goes on, and reports whether it did.
func AwaitAnswer(ctx context.Context, path, id string, n Notifier) bool {
	w, err := readWait(path)
	if err != nil || w.ID != id {
		return false
	}
	timer := time.NewTimer(time.Until(w.Since.Add(AnswerWait)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
	}
	// The Slack DM at 10 minutes goes here once the backend sends it.
	return nudge(path, id, time.Now(), n)
}

// nudge tells the developer, once, that wait id at path still goes on at now, AnswerWait or
// more after it began, and reports whether it did.
func nudge(path, id string, now time.Time, n Notifier) bool {
	var w Wait
	err := flock.Locked(path, waitLockWait, func() error {
		var err error
		if w, err = readWait(path); err != nil {
			return err
		}
		if w.ID != id || w.Nudged || now.Sub(w.Since) < AnswerWait {
			return errNoNudge
		}
		w.Nudged = true
		data, err := json.Marshal(w)
		if err != nil {
			return err
		}
		return config.WriteFileAtomicNoSync(path, data, 0o600)
	})
	if err != nil {
		return false
	}
	n.Notify(w.message(now))
	return true
}

// message is the nudge: "<agent> is waiting for your answer · terma-cli · 2 min".
func (w Wait) message(now time.Time) string {
	where := ""
	if w.Checkout != "" {
		where = " · " + w.Checkout
	}
	return fmt.Sprintf("%s is waiting for your answer%s · %d min", w.Agent, where, int(now.Sub(w.Since)/time.Minute))
}

func readWait(path string) (Wait, error) {
	var w Wait
	data, err := os.ReadFile(path)
	if err != nil {
		return w, err
	}
	return w, json.Unmarshal(data, &w)
}
