package relay

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// HasLegacyOutbox reports whether an older relay's outbox, dir, holds any part, or what was
// set aside of it by a decision that was never recorded (StageLegacyOutbox) does.
func HasLegacyOutbox(dir string) bool {
	for _, d := range []string{dir, dir + legacyStagedSuffix} {
		o := outbox{dir: d}
		routes, _ := o.routes()
		for _, rt := range routes {
			if entries, _ := o.list(rt); len(entries) > 0 {
				return true
			}
		}
	}
	return false
}

// ingestLegacy moves what an older relay queued in Options.LegacyDir into the outbox, once
// there is a decision of whose it is (Options.LegacyOwner). What it held at the decision was
// set aside then, by one rename of the whole outbox (StageLegacyOutbox), and is taken into the
// decision's lane: its account-less routes into it, and a route already in a lane as it is.
// What it queued since, in an outbox of its own, is ambiguous, which never leaves. Each part
// moves by a rename, keeping its name and so its arrival time, which it still ages out by; one
// that can't be read or moved stays, for the next sweep. It runs at every outbox sweep, the
// first as the relay starts, and returns the routes it moved parts to, for their senders to
// queue.
func (r *Relay) ingestLegacy() []route {
	if r.opts.LegacyDir == "" || r.opts.LegacyDir == r.outbox.dir || r.opts.LegacyOwner == nil {
		return nil
	}
	staged := r.opts.LegacyDir + legacyStagedSuffix
	lane := auth.LegacyLane(r.opts.LegacyOwner())
	if lane == "" {
		return nil
	}
	return append(r.moveLegacy(staged, lane), r.moveLegacy(r.opts.LegacyDir, auth.LegacyAccountAmbiguous)...)
}

// legacyStagedSuffix names, beside an older relay's outbox, what it held at the decision.
const legacyStagedSuffix = "-decided-v2"

// StageLegacyOutbox sets the older relay's outbox dir aside as the decision is taken
// (auth.LegacyStage), to be taken in under it, unless something is set aside already: by a
// run that stopped before recording the decision.
func StageLegacyOutbox(dir string) error {
	staged := dir + legacyStagedSuffix
	if _, err := os.Stat(staged); err == nil {
		return nil
	}
	err := os.Rename(dir, staged)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// moveLegacy moves the parts of the older outbox dir into lane.
func (r *Relay) moveLegacy(dir, lane string) []route {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	old := outbox{dir: dir}
	routes, _ := old.routes()
	// Under deliverMu, as a put is, so no empty route is removed between making its
	// directory and moving a part in.
	r.deliverMu.Lock()
	defer r.deliverMu.Unlock()
	var moved []route
	for _, from := range routes {
		entries, err := old.list(from)
		if err != nil || len(entries) == 0 {
			continue
		}
		to := from
		if to.account == "" {
			to.account = lane
		}
		dir := r.outbox.routeDir(to)
		if os.MkdirAll(dir, 0o700) != nil {
			continue
		}
		if from.repo != noRepo {
			if _, err := os.Stat(filepath.Join(dir, identity)); errors.Is(err, fs.ErrNotExist) {
				if b, err := os.ReadFile(filepath.Join(old.routeDir(from), identity)); err == nil {
					_ = config.WriteFileAtomicNoSync(filepath.Join(dir, identity), b, 0o600)
				}
			}
		}
		n := 0
		for _, e := range entries {
			if config.Rename(filepath.Join(old.routeDir(from), e.name), filepath.Join(dir, e.name)) == nil {
				n++
			}
		}
		if n > 0 {
			moved = append(moved, to)
		}
	}
	return moved
}
