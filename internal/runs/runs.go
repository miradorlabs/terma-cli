// Package runs is the run ledger (outside internal/relay so delivery may import it).
// Runs begin at session start/resume; Mixed only turns on and only a new run ends it.
package runs

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// Ledger paths, retention windows (15d outlives 14d spool/outbox plus skew; 90d for Mixed), and lock bounds.
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

func (r run) why() Why {
	if r.MixedWhy.Reason == "" {
		r.MixedWhy.Reason = Crossed
	}
	return r.MixedWhy
}

func (r *run) mark(at time.Time, why Why) {
	if at = at.UTC(); r.MixedAt.IsZero() || at.Before(r.MixedAt) {
		r.MixedAt, r.MixedWhy = at, why
	}
}

// Start records a new clean run of sessionID beginning at at and returns its ID.
func Start(stateDir, sessionID string, at time.Time) (string, error) {
	id := newRunID()
	if err := update(stateDir, sessionID, func(l *ledger) {
		l.Runs = append(l.Runs, run{ID: id, Started: at.UTC()})
		l.active(at)
	}); err != nil {
		return "", err
	}
	return id, nil
}

// Alias records that alias (e.g. a subagent ID) shares sessionID's runs.
func Alias(stateDir, sessionID, alias string) error {
	if !session.ValidID(alias) {
		return fmt.Errorf("runs: invalid alias %q", alias)
	}
	if alias == sessionID {
		return nil
	}
	return editLedger(stateDir, sessionID, false, func(path string, l *ledger) error {
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

// MaxLocations bounds the distinct locations stored per run in the ledger. Touching more
// than MaxLocations distinct locations marks the run Mixed with LocationBound.
const MaxLocations = 64

func touchWith(try bool, stateDir, sessionID, runID string, locations []string) ([]string, error) {
	var merged []string
	err := editLedger(stateDir, sessionID, try, func(path string, l *ledger) error {
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
	if err != nil {
		return nil, err
	}
	return merged, nil
}

// MixedAt reports whether the run of idOrAlias in force at recordTime is Mixed and since when.
// Unreadable ledgers count as Mixed from the start; missing ledgers never do.
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

// Why returns why the run in force at recordTime is Mixed (zero if not or unreadable).
func (s Snapshot) Why(recordTime time.Time) Why {
	if s.unreadable || len(s.runs) == 0 {
		return Why{}
	}
	if r := inForce(s.runs, recordTime); !r.MixedAt.IsZero() {
		return r.why()
	}
	return Why{}
}

// Current returns idOrAlias's current run ID, or false if unrecorded or unreadable.
func Current(stateDir, idOrAlias string) (string, bool) {
	p, ok := Locate(stateDir, idOrAlias)
	return p.Run, ok
}

// Locations returns Current along with the locations that run has been placed in or touched.
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

// Locate reads the ledger once for a hook's current run, locations, Mixed/sidecar state, and aliases.
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
