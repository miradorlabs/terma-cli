// Package runs is the run ledger: for each agent session, its runs, where each has been and
// when each became Mixed (crossed organizations), kept in the state directory long after the
// session's claim has expired, for hooks to tell a crossing by and for the relay and delivery
// to recheck queued records against. It lives outside internal/relay because delivery may
// not import the relay's packages.
//
// A run begins at a session start or resume; a subagent starting is not a new run, and
// its ID shares its session's runs (Alias). Mixed only turns on, and only a new run ends it.
package runs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// Dir is the run ledger's folder under the state directory: one file per session, and
// aliases/ naming each subagent ID's session.
const Dir = "runs"

const aliasesDir = "aliases"

// Retention is how long a session's ledger is kept after its last activity (ledger.Active):
// a run starting, crossing organizations, or recording a location. The relay outbox and
// the spool keep a record for at most 14 days, and a crossing is written here when a hook
// sees it, so every queued record the ledger could drop leaves its queue within 14 days of
// it. The extra day covers the clocks of the processes involved and when Prune happens to
// run: a ledger pruned first would read as never Mixed, and the record would be sent.
// Prune still keeps a ledger a queued record names.
const Retention = 15 * 24 * time.Hour

// lockWait bounds the wait for a session's ledger lock. Every write is a read, a small
// change and a rename; a hook that cannot have the lock gets an error, never a stall.
const lockWait = 2 * time.Second

// ledger is one session's file.
type ledger struct {
	// Runs are in the order they began.
	Runs []run `json:"runs"`
	// Aliases are the subagent IDs that share the session's runs.
	Aliases []string `json:"aliases,omitempty"`
	// Active is when a hook last recorded here a run starting, crossing organizations or
	// touching a new location: what Prune keeps the ledger by.
	Active time.Time `json:"active,omitzero"`
}

// active records activity at t.
func (l *ledger) active(t time.Time) {
	if t = t.UTC(); t.After(l.Active) {
		l.Active = t
	}
}

// lastActive is l's last activity; for a ledger written before Active was, its runs'
// latest start or crossing, else written, its file's time.
func (l ledger) lastActive(written time.Time) time.Time {
	last := l.Active
	for _, r := range l.Runs {
		last = latest(last, r.Started, r.MixedAt)
	}
	if last.IsZero() {
		return written
	}
	return last
}

func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}

// run is one run of a session.
type run struct {
	ID string `json:"id"`
	// Started is when the run began; zero for a run first known by its crossing.
	Started time.Time `json:"started"`
	// MixedAt is when the run became Mixed; zero while it is not.
	MixedAt time.Time `json:"mixed_at,omitzero"`
	// Locations are the distinct locations the run was placed in or seen touching (Touch),
	// in the order first recorded. The ledger does not read them: hooks record and classify
	// them, each a normalised repository ID or a token of their own for outside git.
	Locations []string `json:"locations,omitempty"`
}

// Start records that a new run of sessionID begins at at, a session start or resume, and
// returns its ID. Whatever the previous run was, the new one is not Mixed and has been
// nowhere yet.
func Start(stateDir, sessionID string, at time.Time) (string, error) {
	id, err := newRunID()
	if err != nil {
		return "", err
	}
	err = update(stateDir, sessionID, func(l *ledger) {
		l.Runs = append(l.Runs, run{ID: id, Started: at.UTC()})
		l.active(at)
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// Alias records that alias, a subagent's ID its telemetry may name, shares sessionID's
// runs: a subagent's crossing makes its session's run Mixed, and MixedAt answers for it.
func Alias(stateDir, sessionID, alias string) error {
	if !session.ValidID(alias) {
		return fmt.Errorf("runs: invalid alias %q", alias)
	}
	if alias == sessionID {
		return nil
	}
	return lockedEdit(stateDir, sessionID, func(path string, l *ledger) error {
		if !slices.Contains(l.Aliases, alias) {
			l.Aliases = append(l.Aliases, alias)
			if err := save(path, l); err != nil {
				return err
			}
		}
		pointer := aliasPath(stateDir, alias)
		if err := os.MkdirAll(filepath.Dir(pointer), 0o700); err != nil {
			return err
		}
		return config.WriteFileAtomic(pointer, []byte(sessionID), 0o600)
	})
}

// MarkMixed records that sessionID's current run, the latest to begin, became Mixed at
// at. It only turns on: a run already Mixed keeps the earlier time. A crossing seen before
// any run was recorded starts one that covers every earlier record.
func MarkMixed(stateDir, sessionID string, at time.Time) error {
	id, err := newRunID()
	if err != nil {
		return err
	}
	return update(stateDir, sessionID, func(l *ledger) {
		if len(l.Runs) == 0 {
			l.Runs = append(l.Runs, run{ID: id})
		}
		cur := &l.Runs[len(l.Runs)-1]
		if at = at.UTC(); cur.MixedAt.IsZero() || at.Before(cur.MixedAt) {
			cur.MixedAt = at
		}
		l.active(at)
	})
}

// Touch records that sessionID's run runID was placed in, or seen touching, locations: it
// adds those the run does not record yet, and writes the ledger only when one is new. An
// empty or unknown runID is the current run's. A location seen before any run was recorded
// starts one, as MarkMixed does. It returns every location the run records now, read under
// the lock it writes under: a hook that read the ledger before another hook of the run
// wrote to it sees that hook's locations here.
func Touch(stateDir, sessionID, runID string, locations []string) ([]string, error) {
	if len(locations) == 0 {
		return nil, nil
	}
	var merged []string
	err := lockedEdit(stateDir, sessionID, func(path string, l *ledger) error {
		if len(l.Runs) == 0 {
			id, err := newRunID()
			if err != nil {
				return err
			}
			l.Runs = append(l.Runs, run{ID: id})
		}
		i := slices.IndexFunc(l.Runs, func(r run) bool { return r.ID == runID })
		if runID == "" || i < 0 {
			i = len(l.Runs) - 1
		}
		r := &l.Runs[i]
		known := len(r.Locations)
		for _, loc := range locations {
			if loc != "" && !slices.Contains(r.Locations, loc) {
				r.Locations = append(r.Locations, loc)
			}
		}
		merged = r.Locations
		if len(r.Locations) == known {
			return nil
		}
		l.active(time.Now())
		return save(path, l)
	})
	if err != nil {
		return nil, err
	}
	return merged, nil
}

// MixedAt reports whether the run of idOrAlias, a session ID or one of its aliases, in
// force at recordTime is Mixed, and from when: the caller drops a record attributed
// through its run only at or after that time. The run in force is the latest to begin by
// recordTime; a record older than every run is the earliest's, and one with no time the
// current run's. A ledger that cannot be read counts as Mixed from the start, since it
// may have recorded a crossing; none at all, as for an ID no file can be named after, is
// never Mixed.
func MixedAt(stateDir, idOrAlias string, recordTime time.Time) (time.Time, bool) {
	return Read(stateDir, idOrAlias).MixedAt(recordTime)
}

// Snapshot is a session's runs as one read of its ledger found them (Read), for a caller
// that asks MixedAt of many records of the session, as the relay does of a queue.
type Snapshot struct {
	runs []run
	// unreadable: the ledger could not be read, so every record counts as Mixed.
	unreadable bool
}

// Read reads the ledger of idOrAlias, a session ID or one of its aliases, once.
func Read(stateDir, idOrAlias string) Snapshot {
	if !session.ValidID(idOrAlias) {
		return Snapshot{}
	}
	l, _, err := resolve(stateDir, idOrAlias)
	if err != nil {
		return Snapshot{unreadable: true}
	}
	return Snapshot{runs: l.Runs}
}

// MixedAt is the package's MixedAt, answered from this read of the ledger.
func (s Snapshot) MixedAt(recordTime time.Time) (time.Time, bool) {
	switch {
	case s.unreadable:
		return time.Time{}, true
	case len(s.runs) == 0:
		return time.Time{}, false
	}
	r := inForce(s.runs, recordTime)
	return r.MixedAt, !r.MixedAt.IsZero()
}

// Current is the run idOrAlias, a session ID or one of its aliases, is in now: the latest
// to begin. False when no run of it is recorded, or its ledger cannot be read. Hooks name
// it on what they record, long after the session's claim has expired.
func Current(stateDir, idOrAlias string) (string, bool) {
	run, _, ok := Locations(stateDir, idOrAlias)
	return run, ok
}

// Locations is Current with the locations that run was placed in or seen touching (Touch),
// from one read of the ledger: a hook reads them once, to tell whether the run crosses
// organizations at the locations it sees next.
func Locations(stateDir, idOrAlias string) (run string, locations []string, ok bool) {
	if !session.ValidID(idOrAlias) {
		return "", nil, false
	}
	l, ok, err := resolve(stateDir, idOrAlias)
	if err != nil || !ok || len(l.Runs) == 0 {
		return "", nil, false
	}
	cur := l.Runs[len(l.Runs)-1]
	return cur.ID, cur.Locations, true
}

// Aliases are the subagent IDs that share sessionID's runs (Alias), in the order they were
// recorded: every identity a crossing of its run covers.
func Aliases(stateDir, sessionID string) ([]string, error) {
	path, err := ledgerPath(stateDir, sessionID)
	if err != nil {
		return nil, err
	}
	l, _, err := load(path)
	if err != nil {
		return nil, err
	}
	return slices.Clone(l.Aliases), nil
}

// inForce is the run in force at t among runs, in the order they began.
func inForce(runs []run, t time.Time) run {
	if t.IsZero() {
		return runs[len(runs)-1]
	}
	best, earliest := -1, 0
	for i, r := range runs {
		if !r.Started.After(t) && (best < 0 || !r.Started.Before(runs[best].Started)) {
			best = i
		}
		if r.Started.Before(runs[earliest].Started) {
			earliest = i
		}
	}
	if best < 0 {
		return runs[earliest]
	}
	return runs[best]
}

// resolve reads the ledger idOrAlias names: its own, else its session's through its alias.
func resolve(stateDir, idOrAlias string) (ledger, bool, error) {
	path, err := ledgerPath(stateDir, idOrAlias)
	if err != nil {
		return ledger{}, false, err
	}
	l, ok, err := load(path)
	if err != nil || ok {
		return l, ok, err
	}
	b, err := os.ReadFile(aliasPath(stateDir, idOrAlias))
	if errors.Is(err, fs.ErrNotExist) {
		return ledger{}, false, nil
	}
	if err != nil {
		return ledger{}, false, err
	}
	if path, err = ledgerPath(stateDir, strings.TrimSpace(string(b))); err != nil {
		return ledger{}, false, err
	}
	return load(path)
}

// update applies change to sessionID's ledger, made if there is none, under its lock.
func update(stateDir, sessionID string, change func(*ledger)) error {
	return lockedEdit(stateDir, sessionID, func(path string, l *ledger) error {
		change(l)
		return save(path, l)
	})
}

// lockedEdit runs edit on sessionID's ledger, empty if there is none, holding its lock.
func lockedEdit(stateDir, sessionID string, edit func(path string, l *ledger) error) error {
	path, err := ledgerPath(stateDir, sessionID)
	if err != nil {
		return err
	}
	return flock.Locked(path, lockWait, func() error {
		l, _, err := load(path)
		if err != nil {
			return err
		}
		return edit(path, &l)
	})
}

func ledgerPath(stateDir, sessionID string) (string, error) {
	if !session.ValidID(sessionID) {
		return "", fmt.Errorf("runs: invalid session id %q", sessionID)
	}
	return filepath.Join(stateDir, Dir, sessionID+".json"), nil
}

func aliasPath(stateDir, alias string) string {
	return filepath.Join(stateDir, Dir, aliasesDir, alias)
}

func load(path string) (ledger, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ledger{}, false, nil
	}
	if err != nil {
		return ledger{}, false, err
	}
	var l ledger
	if err := json.Unmarshal(b, &l); err != nil {
		return ledger{}, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return l, true, nil
}

// save writes l durably: a crossing recorded here must survive a crash.
func save(path string, l *ledger) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, b, 0o600)
}

func newRunID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
