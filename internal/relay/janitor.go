package relay

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Bounds on what the relay keeps on disk. A machine offline for weeks, or a project whose
// key is refused for good, fills the queue; these keep it from filling the disk.
const (
	maxQueueBytes = 256 << 20
	maxDeadBytes  = 32 << 20
	maxQueueAge   = 14 * 24 * time.Hour
	// sessionRetention is how long a recorded session directory and route are kept.
	sessionRetention = 30 * 24 * time.Hour
)

// queued is one file the janitor weighs.
type queued struct {
	path    string
	size    int64
	modTime time.Time
}

// sweep enforces the bounds once: bodies older than maxQueueAge go, then the oldest
// bodies until the queue fits maxQueueBytes, then the oldest refused bodies until dead/
// fits maxDeadBytes; and session records older than sessionRetention. It returns how
// many queued bodies it dropped. The inbox counts toward the bound but only the outbox
// is trimmed for size: the router owns the inbox, and what it holds is seconds old.
func sweep(dir string, now time.Time) (dropped int) {
	var outbox []queued
	var total int64
	inbox := collect(filepath.Join(dir, inboxDir))
	for _, q := range inbox {
		total += q.size
	}
	rs, _ := routes(filepath.Join(dir, outboxDir))
	for _, r := range rs {
		outbox = append(outbox, collect(filepath.Join(dir, outboxDir, r))...)
	}
	slices.SortFunc(outbox, func(a, b queued) int { return a.modTime.Compare(b.modTime) })
	var kept []queued
	for _, q := range outbox {
		if now.Sub(q.modTime) > maxQueueAge {
			if remove(q.path) {
				dropped++
			}
			continue
		}
		kept = append(kept, q)
		total += q.size
	}
	for _, q := range kept {
		if total <= maxQueueBytes {
			break
		}
		if remove(q.path) {
			dropped++
		}
		total -= q.size
	}

	dead := collect(filepath.Join(dir, deadDir))
	slices.SortFunc(dead, func(a, b queued) int { return a.modTime.Compare(b.modTime) })
	var deadTotal int64
	for _, q := range dead {
		deadTotal += q.size
	}
	for _, q := range dead {
		if deadTotal <= maxDeadBytes && now.Sub(q.modTime) <= maxQueueAge {
			continue
		}
		remove(q.path)
		deadTotal -= q.size
	}

	for _, sub := range []string{sessionsDir, routesDir} {
		for _, q := range collect(filepath.Join(dir, sub)) {
			if now.Sub(q.modTime) > sessionRetention {
				remove(q.path)
			}
		}
	}
	return dropped
}

// collect lists dir's regular files, skipping temporary ones.
func collect(dir string) []queued {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []queued
	for _, de := range des {
		if !de.Type().IsRegular() || strings.HasPrefix(de.Name(), ".") {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		out = append(out, queued{path: filepath.Join(dir, de.Name()), size: info.Size(), modTime: info.ModTime()})
	}
	return out
}

// remove deletes path, true when this call removed it.
func remove(path string) bool {
	return os.Remove(path) == nil
}

// backlog counts the bodies waiting in the inbox and every outbox.
func backlog(dir string) int {
	n := len(collect(filepath.Join(dir, inboxDir)))
	rs, _ := routes(filepath.Join(dir, outboxDir))
	for _, r := range rs {
		n += len(collect(filepath.Join(dir, outboxDir, r)))
	}
	return n
}
