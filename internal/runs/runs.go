// Package runs is the run ledger: each session's runs, where each has been, and when each
// became Mixed (crossed organizations). Hooks tell a crossing by it; the relay and delivery
// recheck queued records against it. It outlives the session's claim, and sits outside
// internal/relay so that delivery may import it.
//
// A run begins at a session start or resume. A subagent shares its session's runs (Alias).
// Mixed only turns on, and only a new run ends it.
package runs

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// Dir is the ledger's folder under the state directory: a file per session, and aliases/.
const Dir = "runs"

const aliasesDir = "aliases"

// Retention is how long a ledger is kept after its last activity (ledger.Active). Queued
// records leave the relay outbox and the spool within 14 days; the extra day covers clock
// skew and when Prune runs. A ledger pruned too early reads as never Mixed. Prune also keeps
// any ledger a queued record names.
const Retention = 15 * 24 * time.Hour

// MixedRetention is Retention for a ledger whose current run is Mixed: a session idle past
// Retention may go on without a new run, and must still read Mixed.
const MixedRetention = 90 * 24 * time.Hour

// RefreshEvery is how often hooks renew a ledger's activity (Refresh). A run that stays put
// records nothing new; unrenewed, its ledger and any crossing in it would be pruned.
const RefreshEvery = 24 * time.Hour

// lockWait bounds the wait for a ledger's lock: a hook gets an error, never a stall.
const lockWait = 2 * time.Second

// ledger is one session's file.
type ledger struct {
	// Runs are in the order they began.
	Runs []run `json:"runs"`
	// Aliases are the subagent IDs that share the session's runs.
	Aliases []string `json:"aliases,omitempty"`
	// Active is the last run start, crossing, new location or Refresh. Prune goes by it.
	Active time.Time `json:"active,omitzero"`
}

// active records activity at t.
func (l *ledger) active(t time.Time) {
	if t = t.UTC(); t.After(l.Active) {
		l.Active = t
	}
}

// lastActive is l's last activity. A ledger from before Active falls back to its runs'
// latest start or crossing, then to written, its file's time.
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

// Reason is why a run turned Mixed, which doctor and a flush show, so that a run the scan
// could not read can be told from one that crossed.
type Reason string

const (
	// Crossed: the run touched another organization's checkout. A ledger written before
	// reasons were recorded reads as this.
	Crossed Reason = "crossed"
	// UnknownReach: a shell command's reach could not be resolved.
	UnknownReach Reason = "unknown_reach"
	// LocationBound: the run touched more locations than the ledger keeps for it.
	LocationBound Reason = "location_bound"
	// Aside: the ledger was busy, so the crossing went to its sidecar (MarkAside), or its
	// placement alone kept it; why it crossed is in the hook's log.
	Aside Reason = "aside"
)

// Why is a Mixed mark's reason, and where it happened: for Crossed the checkout in another
// organization, as the ledger records a location; for UnknownReach the command's first
// words. Never a file's contents.
type Why struct {
	Reason Reason `json:"reason,omitempty"`
	Where  string `json:"where,omitempty"`
}

// run is one run of a session.
type run struct {
	ID string `json:"id"`
	// Started is when the run began; zero for a run first known by its crossing.
	Started time.Time `json:"started"`
	// MixedAt is when the run became Mixed; zero while it is not. MixedWhy is why, absent
	// from a ledger written before reasons were recorded (why).
	MixedAt  time.Time `json:"mixed_at,omitzero"`
	MixedWhy Why       `json:"mixed_why,omitzero"`
	// Locations are where the run was placed or seen touching (Touch), first seen first:
	// normalised repository IDs, or the hooks' token for outside git. Only hooks read them.
	Locations []string `json:"locations,omitempty"`
}

// why is why r turned Mixed: Crossed for a mark that records no reason.
func (r run) why() Why {
	if r.MixedWhy.Reason == "" {
		return Why{Reason: Crossed, Where: r.MixedWhy.Where}
	}
	return r.MixedWhy
}

// mark makes r Mixed at at for why, unless it already is from no later: the earlier mark
// wins, with its reason.
func (r *run) mark(at time.Time, why Why) {
	if at = at.UTC(); r.MixedAt.IsZero() || at.Before(r.MixedAt) {
		r.MixedAt, r.MixedWhy = at, why
	}
}

// Start records a new run of sessionID beginning at at (a session start or resume) and
// returns its ID. The new run is not Mixed and has been nowhere.
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

// Alias records that alias, a subagent ID its telemetry may name, shares sessionID's runs,
// so MixedAt answers for it too.
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

// MarkMixed records that sessionID's current run became Mixed at at, for why; an earlier
// time wins, with its reason. With no run recorded yet, it starts one that covers every
// earlier record.
func MarkMixed(stateDir, sessionID string, at time.Time, why Why) error {
	id, err := newRunID()
	if err != nil {
		return err
	}
	return update(stateDir, sessionID, func(l *ledger) {
		if len(l.Runs) == 0 {
			l.Runs = append(l.Runs, run{ID: id})
		}
		l.Runs[len(l.Runs)-1].mark(at, why)
		l.active(at)
	})
}

// Touch adds locations to sessionID's run runID (the current run when runID is empty or
// unknown), writing only when one is new; with no run recorded it starts one. It returns
// all the run's locations, read under the write lock: those another hook recorded since
// this one read the ledger included.
func Touch(stateDir, sessionID, runID string, locations []string) ([]string, error) {
	if len(locations) == 0 {
		return nil, nil
	}
	return touchWith(lockedEdit, stateDir, sessionID, runID, locations)
}

// TryTouch is Touch only if the ledger's lock is free now: it never waits. With no location
// to add, it still folds the sidecar in (MarkAside).
func TryTouch(stateDir, sessionID, runID string, locations []string) ([]string, error) {
	return touchWith(tryEdit, stateDir, sessionID, runID, locations)
}

func touchWith(edit func(stateDir, sessionID string, edit func(path string, l *ledger) error) error, stateDir, sessionID, runID string, locations []string) ([]string, error) {
	var merged []string
	err := edit(stateDir, sessionID, func(path string, l *ledger) error {
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

// MixedAt reports whether the run of idOrAlias (a session or one of its aliases) in force
// at recordTime is Mixed, and since when: the caller drops a record from then on. The run
// in force is the latest to begin by recordTime; an older record's is the earliest, and
// one with no time the current. An unreadable ledger is Mixed from the start; none, never.
func MixedAt(stateDir, idOrAlias string, recordTime time.Time) (time.Time, bool) {
	return Read(stateDir, idOrAlias).MixedAt(recordTime)
}

// Snapshot is one read of a session's ledger (Read), for a caller asking MixedAt of many
// records, as the relay's queue recheck does.
type Snapshot struct {
	runs []run
	// unreadable: the ledger could not be read, so every record counts as Mixed.
	unreadable bool
}

// Read reads the ledger of idOrAlias, a session ID or one of its aliases, once, with its
// sidecar (MarkAside).
func Read(stateDir, idOrAlias string) Snapshot {
	if !session.ValidID(idOrAlias) {
		return Snapshot{}
	}
	l, _, _, err := resolve(stateDir, idOrAlias)
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

// Why is why the run in force at recordTime is Mixed (MixedAt): zero for one that is not,
// and for a ledger that cannot be read.
func (s Snapshot) Why(recordTime time.Time) Why {
	if s.unreadable || len(s.runs) == 0 {
		return Why{}
	}
	if r := inForce(s.runs, recordTime); !r.MixedAt.IsZero() {
		return r.why()
	}
	return Why{}
}

// Current is the run idOrAlias is in now, the latest to begin; false when none is recorded
// or the ledger cannot be read.
func Current(stateDir, idOrAlias string) (string, bool) {
	p, ok := Locate(stateDir, idOrAlias)
	return p.Run, ok
}

// Locations is Current with the locations that run was placed in or seen touching (Touch).
func Locations(stateDir, idOrAlias string) (run string, locations []string, ok bool) {
	p, ok := Locate(stateDir, idOrAlias)
	return p.Run, p.Locations, ok
}

// Position is a session's current run, from one read of its ledger (Locate).
type Position struct {
	// Run is the current run, and Locations where it has been.
	Run       string
	Locations []string
	// Active is the ledger's last activity, which Prune goes by.
	Active time.Time
	// MixedAt is when Run became Mixed; zero while it is not.
	MixedAt time.Time
	// Aside: a sidecar records a crossing the ledger has yet to fold in (MarkAside).
	Aside bool
	// Aliases are the subagent IDs sharing the session's runs (Aliases).
	Aliases []string
}

// Locate reads the ledger once for what a hook needs: the current run, where it has been,
// whether it is Mixed (its sidecar included), whether the ledger is due a Refresh, and the
// subagents sharing the session's runs.
func Locate(stateDir, idOrAlias string) (Position, bool) {
	if !session.ValidID(idOrAlias) {
		return Position{}, false
	}
	l, ok, aside, err := resolve(stateDir, idOrAlias)
	if err != nil || !ok || len(l.Runs) == 0 {
		return Position{}, false
	}
	cur := l.Runs[len(l.Runs)-1]
	return Position{Run: cur.ID, Locations: cur.Locations, Active: l.lastActive(time.Time{}), MixedAt: cur.MixedAt,
		Aside: aside, Aliases: l.Aliases}, true
}

// Refresh renews sessionID's ledger at now if its last activity is older than
// RefreshEvery; one with no run, or none, is left alone. Hooks call it only when Locate
// found it due, so at most once a day.
func Refresh(stateDir, sessionID string, now time.Time) error {
	path, err := ledgerPath(stateDir, sessionID)
	if err != nil {
		return err
	}
	return flock.Locked(path, lockWait, func() error {
		l, _, err := load(path)
		if err != nil {
			return err
		}
		if err := fold(stateDir, sessionID, path, &l); err != nil {
			return err
		}
		if len(l.Runs) == 0 || now.Sub(l.lastActive(time.Time{})) <= RefreshEvery {
			return nil
		}
		l.active(now)
		return save(path, &l)
	})
}

// Aliases are the subagent IDs sharing sessionID's runs (Alias), in recorded order.
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
