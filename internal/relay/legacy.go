package relay

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// HasLegacyOutbox reports whether an older relay's outbox, dir, holds any part.
func HasLegacyOutbox(dir string) bool {
	o := outbox{dir: dir}
	routes, _ := o.routes()
	for _, rt := range routes {
		if entries, _ := o.list(rt); len(entries) > 0 {
			return true
		}
	}
	return false
}

// ingestLegacy moves what an older relay queued in Options.LegacyDir into the outbox, once
// there is a decision of whose it is: its account-less routes into that decision's lane,
// and a route already in a lane as it is. Each part moves by a rename, keeping its name and
// so its arrival time, which it still ages out by. It runs at every outbox sweep, the first
// as the relay starts, and returns the routes it moved parts to, for their senders to
// queue.
func (r *Relay) ingestLegacy() []route {
	if r.opts.LegacyDir == "" || r.opts.LegacyDir == r.outbox.dir || r.opts.LegacyOwner == nil {
		return nil
	}
	if _, err := os.Stat(r.opts.LegacyDir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	lane := auth.LegacyLane(r.opts.LegacyOwner())
	if lane == "" {
		return nil
	}
	old := outbox{dir: r.opts.LegacyDir}
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
