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
	"github.com/miradorlabs/terma-cli/internal/session"
)

// Ledger paths, retention windows (15d outlives 14d spool/outbox plus skew; 90d for Mixed),
// refresh cadence, and lock bounds.
const (
	Dir, aliasesDir = "runs", "aliases"
	Retention       = 15 * 24 * time.Hour
	MixedRetention  = 90 * 24 * time.Hour
	RefreshEvery    = 24 * time.Hour
	lockWait        = 2 * time.Second
)

type ledger struct {
	Runs    []run     `json:"runs"`
	Aliases []string  `json:"aliases,omitempty"`
	Active  time.Time `json:"active,omitzero"`
}

func (l *ledger) active(t time.Time) {
	if t = t.UTC(); t.After(l.Active) {
		l.Active = t
	}
}

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

// Reason is why a run turned Mixed (shown by doctor and flush).
type Reason string

// Reasons a run is marked Mixed.
const (
	Crossed       Reason = "crossed"
	UnknownReach  Reason = "unknown_reach"
	LocationBound Reason = "location_bound"
	Aside         Reason = "aside"
)

// Why is a Mixed mark's reason and sanitized location/command prefix (never file contents).
type Why struct {
	Reason Reason `json:"reason,omitempty"`
	Where  string `json:"where,omitempty"`
}

type run struct {
	ID        string    `json:"id"`
	Started   time.Time `json:"started"`
	MixedAt   time.Time `json:"mixed_at,omitzero"`
	MixedWhy  Why       `json:"mixed_why,omitzero"`
	Locations []string  `json:"locations,omitempty"`
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

// MaxLocations bounds the distinct locations stored per run in the ledger. Touching more
// than MaxLocations distinct locations marks the run Mixed with LocationBound.
const MaxLocations = 64

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
		prevMixedAt, prevMixedWhy := r.MixedAt, r.MixedWhy
		exceeded := false
		for _, loc := range locations {
			if loc == "" || slices.Contains(r.Locations, loc) {
				continue
			}
			if len(r.Locations) >= MaxLocations {
				exceeded = true
				break
			}
			r.Locations = append(r.Locations, loc)
		}
		if exceeded {
			r.mark(time.Now(), Why{Reason: LocationBound})
		}
		merged = r.Locations
		if len(r.Locations) == known && r.MixedAt.Equal(prevMixedAt) && r.MixedWhy == prevMixedWhy {
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
	Run       string
	Locations []string
	Active    time.Time
	MixedAt   time.Time
	Aside     bool
	Aliases   []string
}

// Locate reads the ledger once for what a hook needs (current run, locations, Mixed/sidecar
// state, activity, and aliases).
func Locate(stateDir, idOrAlias string) (Position, bool) {
	if !session.ValidID(idOrAlias) {
		return Position{}, false
	}
	l, ok, aside, err := resolve(stateDir, idOrAlias)
	if err != nil || !ok || len(l.Runs) == 0 {
		return Position{}, false
	}
	cur := l.Runs[len(l.Runs)-1]
	return Position{Run: cur.ID, Locations: cur.Locations, Active: l.lastActive(time.Time{}), MixedAt: cur.MixedAt, Aside: aside, Aliases: l.Aliases}, true
}

// Refresh renews sessionID's ledger at now if its last activity is older than RefreshEvery.
func Refresh(stateDir, sessionID string, now time.Time) error {
	return lockedEdit(stateDir, sessionID, func(path string, l *ledger) error {
		if len(l.Runs) == 0 || now.Sub(l.lastActive(time.Time{})) <= RefreshEvery {
			return nil
		}
		l.active(now)
		return save(path, l)
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
