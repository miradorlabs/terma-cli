// Package runs is the run ledger (outside internal/relay so delivery may import it).
// Runs begin at session start/resume; Mixed only turns on and only a new run ends it.
package runs

import (
	"cmp"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/session"
)

// Ledger paths, retention windows (15d outlives 14d spool/outbox plus skew; 90d for Mixed), and lock bounds.
const (
	Dir, aliasesDir = "runs", "aliases"
	Retention       = 15 * 24 * time.Hour
	MixedRetention  = 90 * 24 * time.Hour
	RefreshEvery    = 24 * time.Hour
	lockWait        = 2 * time.Second
	MaxLocations    = 64 // exceeding it marks the run Mixed (LocationBound)
)

type ledger struct {
	Runs    []run     `json:"runs"`
	Aliases []string  `json:"aliases,omitempty"`
	Active  time.Time `json:"active,omitzero"`
}

func (l *ledger) active(t time.Time) { l.Active = latest(l.Active, t.UTC()) }
func (l ledger) lastActive(written time.Time) time.Time {
	last := l.Active
	for _, r := range l.Runs {
		last = latest(last, r.Started, r.MixedAt)
	}
	return cmp.Or(last, written)
}
func latest(ts ...time.Time) (out time.Time) {
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

func (r run) why() Why { r.MixedWhy.Reason = cmp.Or(r.MixedWhy.Reason, Crossed); return r.MixedWhy }
func (r *run) mark(at time.Time, why Why) {
	if at = at.UTC(); r.MixedAt.IsZero() || at.Before(r.MixedAt) {
		r.MixedAt, r.MixedWhy = at, why
	}
}

// Start records a new clean run of sessionID beginning at at and returns its ID.
func Start(stateDir, sessionID string, at time.Time) (string, error) {
	id := newRunID()
	err := update(stateDir, sessionID, func(l *ledger) {
		l.Runs = append(l.Runs, run{ID: id, Started: at.UTC()})
		slices.SortStableFunc(l.Runs, func(a, b run) int { return a.Started.Compare(b.Started) })
		l.active(at)
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// Alias records that alias (e.g. a subagent ID) shares sessionID's runs.
func Alias(stateDir, sessionID, alias string) error { return linkAlias(stateDir, sessionID, alias) }

// MarkMixed marks sessionID's current run Mixed at at (earlier time wins); starts one if absent.
func MarkMixed(stateDir, sessionID string, at time.Time, why Why) error {
	return update(stateDir, sessionID, func(l *ledger) {
		if len(l.Runs) == 0 {
			l.Runs = append(l.Runs, run{ID: newRunID()})
		}
		l.Runs[len(l.Runs)-1].mark(at, why)
		l.active(at)
	})
}

// Touch adds locations to runID (or current run) and returns all locations under the write lock.
func Touch(stateDir, sessionID, runID string, locations []string) ([]string, error) {
	if len(locations) == 0 {
		return nil, nil
	}
	return touchWith(false, stateDir, sessionID, runID, locations)
}

// TryTouch is Touch only if the lock is immediately free; also folds any sidecar marks.
func TryTouch(stateDir, sessionID, runID string, locations []string) ([]string, error) {
	return touchWith(true, stateDir, sessionID, runID, locations)
}
func touchWith(try bool, stateDir, sessionID, runID string, locations []string) (merged []string, err error) {
	err = editLedger(stateDir, sessionID, try, func(path string, l *ledger) error {
		if len(l.Runs) == 0 {
			l.Runs = append(l.Runs, run{ID: newRunID()})
		}
		i := slices.IndexFunc(l.Runs, func(r run) bool { return r.ID == runID })
		if runID == "" || i < 0 {
			i = len(l.Runs) - 1
		}
		r := &l.Runs[i]
		known, prevMixedAt, prevMixedWhy := len(r.Locations), r.MixedAt, r.MixedWhy
		for _, loc := range locations {
			if loc == "" || slices.Contains(r.Locations, loc) {
				continue
			}
			if len(r.Locations) >= MaxLocations {
				r.mark(time.Now(), Why{Reason: LocationBound})
				break
			}
			r.Locations = append(r.Locations, loc)
		}
		merged = r.Locations
		if len(r.Locations) == known && r.MixedAt.Equal(prevMixedAt) && r.MixedWhy == prevMixedWhy {
			return nil
		}
		l.active(time.Now())
		return save(path, l)
	})
	return merged, err
}

// MixedAt reports whether the run of idOrAlias in force at recordTime is Mixed and since when.
func MixedAt(stateDir, idOrAlias string, recordTime time.Time) (time.Time, bool) {
	return Read(stateDir, idOrAlias).MixedAt(recordTime)
}

// Snapshot is one read of a session's ledger (Read) for repeated MixedAt checks.
type Snapshot struct {
	runs       []run
	unreadable bool
}

// Read reads idOrAlias's ledger once along with any sidecar marks (MarkAside).
func Read(stateDir, idOrAlias string) Snapshot {
	if !session.ValidID(idOrAlias) {
		return Snapshot{}
	}
	l, _, _, err := resolve(stateDir, idOrAlias)
	return Snapshot{runs: l.Runs, unreadable: err != nil}
}

// MixedAt is the package's MixedAt, answered from this read of the ledger.
func (s Snapshot) MixedAt(recordTime time.Time) (time.Time, bool) {
	if s.unreadable || len(s.runs) == 0 {
		return time.Time{}, s.unreadable
	}
	r := inForce(s.runs, recordTime)
	return r.MixedAt, !r.MixedAt.IsZero()
}

// MixedInterval reports whether any run intersecting [start, end] is Mixed at or before
// end, returning the earliest MixedAt across all intersecting Mixed runs. An unreadable
// ledger fails closed (zero time, true).
func (s Snapshot) MixedInterval(start, end time.Time) (time.Time, bool) {
	_, at, mixed := s.MixedRun(start, end)
	return at, mixed
}

// MixedRun is MixedInterval that also names the run whose MixedAt it returns: empty when
// the ledger is unreadable, so a caller scoping by run covers them all.
func (s Snapshot) MixedRun(start, end time.Time) (string, time.Time, bool) {
	if s.unreadable || len(s.runs) == 0 {
		return "", time.Time{}, s.unreadable
	}
	if start.IsZero() && end.IsZero() {
		if r := inForce(s.runs, time.Time{}); !r.MixedAt.IsZero() {
			return r.ID, r.MixedAt, true
		}
		return "", time.Time{}, false
	}
	if end.IsZero() || (!start.IsZero() && end.Before(start)) {
		end = start
	}
	if start.IsZero() {
		start = end
	}
	ordered := slices.Clone(s.runs)
	slices.SortStableFunc(ordered, func(a, b run) int { return a.Started.Compare(b.Started) })
	var earliest time.Time
	var id string
	found := false
	for i, r := range ordered {
		if i > 0 && end.Before(r.Started) {
			continue
		}
		if i+1 < len(ordered) && !start.Before(ordered[i+1].Started) {
			continue
		}
		if r.MixedAt.IsZero() || end.Before(r.MixedAt) {
			continue
		}
		if !found || r.MixedAt.Before(earliest) {
			id, earliest, found = r.ID, r.MixedAt, true
		}
	}
	return id, earliest, found
}

// Why returns why the run in force at recordTime is Mixed (zero if not or unreadable).
func (s Snapshot) Why(recordTime time.Time) Why {
	if !s.unreadable && len(s.runs) > 0 {
		if r := inForce(s.runs, recordTime); !r.MixedAt.IsZero() {
			return r.why()
		}
	}
	return Why{}
}

// Current returns idOrAlias's current run ID, or false if unrecorded or unreadable.
func Current(stateDir, idOrAlias string) (string, bool) {
	p, ok := Locate(stateDir, idOrAlias)
	return p.Run, ok
}

// Locations returns Current along with the locations that run has been placed in or touched.
func Locations(stateDir, idOrAlias string) (string, []string, bool) {
	p, ok := Locate(stateDir, idOrAlias)
	return p.Run, p.Locations, ok
}

// Position is a session's current run, from one read of its ledger (Locate).
type Position struct {
	Run                string
	Locations, Aliases []string
	Active, MixedAt    time.Time
	Aside              bool
}

// Locate reads the ledger once for a hook's current run, locations, Mixed/sidecar state, and aliases.
func Locate(stateDir, idOrAlias string) (Position, bool) {
	if l, ok, aside, err := resolve(stateDir, idOrAlias); err == nil && ok && len(l.Runs) > 0 {
		cur := l.Runs[len(l.Runs)-1]
		return Position{Run: cur.ID, Locations: cur.Locations, Active: l.lastActive(time.Time{}), MixedAt: cur.MixedAt, Aside: aside, Aliases: l.Aliases}, true
	}
	return Position{}, false
}

// Refresh renews sessionID's ledger at now if its last activity is older than RefreshEvery.
func Refresh(stateDir, sessionID string, now time.Time) error {
	return editLedger(stateDir, sessionID, false, func(path string, l *ledger) error {
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
	return slices.Clone(l.Aliases), err
}
func inForce(runs []run, t time.Time) run { return runs[inForceIdx(runs, t)] }
func inForceIdx(runs []run, t time.Time) int {
	if t.IsZero() {
		return len(runs) - 1
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
	if best >= 0 {
		return best
	}
	return earliest
}
