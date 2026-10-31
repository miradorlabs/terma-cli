package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
)

// AnswerWait is how long a session waits for the developer before they are nudged.
const AnswerWait = 2 * time.Minute

// waitLockWait bounds a hook's wait for the record a check holds.
const waitLockWait = time.Second

// Wait is a session waiting for the developer's answer or permission, from the hook that saw
// it begin until the developer's next sign of life ends it (EndWait), or a check sees one no
// hook reports (answered).
type Wait struct {
	// ID tells this wait from the session's next, so a check for one never nudges about another.
	ID    string    `json:"id"`
	Since time.Time `json:"since"`
	// Agent is the agent as prose names it, and Checkout the folder it works in.
	Agent    string `json:"agent"`
	Checkout string `json:"checkout,omitempty"`
	// Transcript is the session's transcript, TranscriptSize its size as the wait began.
	Transcript     string `json:"transcript,omitempty"`
	TranscriptSize int64  `json:"transcript_size,omitempty"`
	// AgentPID is the agent's process (Watch), and Children those it ran as the wait began.
	AgentPID int   `json:"agent_pid,omitempty"`
	Children []int `json:"children,omitempty"`
	// Nudged is set once the developer has been told.
	Nudged bool `json:"nudged,omitempty"`
}

// Processes is what a wait's check asks of the machine's processes.
type Processes interface {
	// Alive reports whether pid runs, true where that cannot be told.
	Alive(pid int) bool
	// Children are the running processes pid started; ok is false where they cannot be listed.
	Children(pid int) (pids []int, ok bool)
}

// Machine is this machine's processes, as procinfo sees them.
type Machine struct{}

// Alive reports whether pid runs, true where that cannot be told.
func (Machine) Alive(pid int) bool { return procinfo.Alive(pid) }

// Children are the running processes pid started; ok is false where they cannot be listed.
func (Machine) Children(pid int) ([]int, bool) { return procinfo.Children(pid) }

// errNoNudge is a wait that is over, another, already told, or not yet due.
var errNoNudge = errors.New("no nudge due")

// Watch has the wait watch the agent process pid, and the processes it runs now: where they
// cannot be listed, it watches none, and only the transcript and hooks end the wait.
func (w *Wait) Watch(pid int, p Processes) {
	if pid <= 0 {
		return
	}
	if children, ok := p.Children(pid); ok {
		w.AgentPID, w.Children = pid, children
	}
}

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
// developer through n if it still goes on, by its record and p, and reports whether it did.
func AwaitAnswer(ctx context.Context, path, id string, n Notifier, p Processes) bool {
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
	return nudge(path, id, time.Now(), n, p)
}

// nudge tells the developer, once, that wait id at path still goes on at now, AnswerWait or
// more after it began, and reports whether it did.
func nudge(path, id string, now time.Time, n Notifier, p Processes) bool {
	var w Wait
	err := flock.Locked(path, waitLockWait, func() error {
		var err error
		if w, err = readWait(path); err != nil {
			return err
		}
		if w.ID != id || w.Nudged || now.Sub(w.Since) < AnswerWait || w.answered(p) {
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

// answered reports the answers no hook reports: a prompt rejected or dismissed ends the turn
// as an interrupt, with no PostToolUse or Stop, but the agent writes it to the transcript; an
// approved tool has its PostToolUse only once it ends, but runs as a new child of the agent;
// and an agent that has gone waits for nobody.
func (w Wait) answered(p Processes) bool {
	if w.Transcript != "" {
		if info, err := os.Stat(w.Transcript); err == nil && info.Size() > w.TranscriptSize {
			return true
		}
	}
	if w.AgentPID <= 0 {
		return false
	}
	if !p.Alive(w.AgentPID) {
		return true
	}
	children, ok := p.Children(w.AgentPID)
	return ok && slices.ContainsFunc(children, func(c int) bool { return !slices.Contains(w.Children, c) })
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
