// Package claim is how hooks tell the local relay which sessions belong to an opted-in
// repository. A hook in a repository with a project binding writes one small file per
// session; the relay forwards an agent's telemetry only for sessions it finds here.
//
// It is its own package, with no OTLP dependency, because every hook imports it and
// the hook path has a latency budget; the relay is the only reader.
package claim

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// DirName is the relay's directory under the config dir. It holds the claims, the
// local token the agents' exporters present, the address the relay listens on, its
// single-instance lock and the stats it leaves behind.
const DirName = "relay"

const (
	claimsDir = "claims"
	tokenFile = "token"
)

// TTL is how long a claim stays valid after its last write. A hook refreshes it on
// every event of a live session (see Refresh), so a session idle this long has ended;
// it matches the hooks' ActiveTTL.
const TTL = 4 * time.Hour

// Refresh is how old a claim may be before a hook rewrites it. Below it a hook only
// stats the file, so a session's hundreds of tool calls cost one write every few
// minutes, not one each.
const Refresh = 5 * time.Minute

// Claim says which project a session's telemetry belongs to. Its top-level fields are
// the session's latest placement; Placements keeps the earlier ones too.
type Claim struct {
	ProjectID string    `json:"project_id"`
	Tool      string    `json:"tool,omitempty"`
	Repo      string    `json:"repo,omitempty"`
	Worktree  string    `json:"worktree,omitempty"`
	ClaimedAt time.Time `json:"claimed_at"`
	// PIDs are the processes the claiming hooks ran under: the agent is one of them.
	// The relay forwards a record only from a process named here, so the same session
	// resumed by another process elsewhere — where no hook of this repository runs —
	// is not covered. Every hook of the session adds its own; the most recent maxPIDs
	// are kept. Empty (a platform where they cannot be read) matches any sender.
	PIDs []int `json:"pids,omitempty"`
	// Placements are where the session has run, oldest first, the last being the
	// top-level fields. A session keeps its id across `claude --resume` and `codex
	// resume` in any directory: resumed in another bound repository, it gets a second
	// placement, and the first run's records — from its own processes, or stamped before
	// the move — still go to the first run's project, however late they arrive. Empty in
	// a claim an earlier build wrote: the top-level fields are then its one placement.
	Placements []Placement `json:"placements,omitempty"`
}

// Placement is one run of a session in one repository.
type Placement struct {
	ProjectID string    `json:"project_id"`
	Tool      string    `json:"tool,omitempty"`
	Repo      string    `json:"repo,omitempty"`
	Worktree  string    `json:"worktree,omitempty"`
	PIDs      []int     `json:"pids,omitempty"`
	Since     time.Time `json:"since"`
}

// maxPIDs bounds a claim's process list: a few runs of one session, each with its
// chain of ancestors.
const maxPIDs = 64

// maxPlacements bounds a session's placement history.
const maxPlacements = 8

// Covers reports whether pid may send under this claim's latest placement.
func (c Claim) Covers(pid int) bool {
	return covers(c.PIDs, pid)
}

// covers reports whether a placement naming pids covers a record pid sent. A placement
// that names none (a platform where a hook cannot read its processes) covers any
// sender; one that names some covers only those — never a sender the relay could not
// identify (pid 0), which would otherwise let a session resumed elsewhere through.
func covers(pids []int, pid int) bool {
	if len(pids) == 0 {
		return true
	}
	return pid != 0 && slices.Contains(pids, pid)
}

func (c Claim) placements() []Placement {
	if len(c.Placements) > 0 {
		return c.Placements
	}
	return []Placement{{ProjectID: c.ProjectID, Tool: c.Tool, Repo: c.Repo, Worktree: c.Worktree, PIDs: c.PIDs}}
}

// At is the claim as it applies to a record pid sent, stamped at (zero: unknown): the
// placement whose processes include pid — the latest to start by at, when several do
// (one process serving more than one run, such as Codex's app-server) — as a claim of
// its own. False when no placement covers pid: the session was resumed by a process
// no hook of a bound repository ran under.
func (c Claim) At(pid int, at time.Time) (Claim, bool) {
	var best *Placement
	all := c.placements()
	for i := range all {
		p := &all[i]
		if !covers(p.PIDs, pid) {
			continue
		}
		// A later placement that had started by the record's time wins; with no time,
		// the latest does.
		if best == nil || at.IsZero() || !p.Since.After(at) {
			best = p
		}
	}
	if best == nil {
		return Claim{}, false
	}
	return Claim{ProjectID: best.ProjectID, Tool: best.Tool, Repo: best.Repo, Worktree: best.Worktree,
		PIDs: best.PIDs, ClaimedAt: c.ClaimedAt, Placements: c.Placements}, true
}

// Dir is the relay directory, under the config dir.
func Dir() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, DirName), nil
}

// Enabled reports whether this machine exports through the relay: `terma relay setup`
// wrote its token. Without it hooks write no claims — nothing would read them.
func Enabled() bool {
	dir, err := Dir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(dir, tokenFile))
	return err == nil
}

// DefaultAddr is where the relay listens unless `terma relay setup --addr` said
// otherwise. It is fixed, because the agents' exporter configuration is static.
const DefaultAddr = "127.0.0.1:43180"

// TokenPath is where the relay's local token lives.
func TokenPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, tokenFile), nil
}

func path(sessionID string) (string, bool) {
	if !session.ValidID(sessionID) {
		return "", false
	}
	dir, err := Dir()
	if err != nil {
		return "", false
	}
	return filepath.Join(dir, claimsDir, sessionID+".json"), true
}

// lockWait is how long a hook waits for another hook of the same session to finish
// its read-merge-write of the claim: the session store's policy (internal/session). A
// lock that cannot be had costs the write its exclusivity, never the write itself — a
// hook must not lose its claim to a wedged process. A variable so a test of the
// exclusion is not decided by a loaded machine.
var lockWait = 250 * time.Millisecond

// Write records that sessionID belongs to c.ProjectID, merging c.PIDs into the
// processes already named, unless a claim for the same project naming them was
// written less than Refresh ago. It reports whether it wrote. Errors are swallowed: a
// hook never fails for want of a claim, the session is only not exported.
//
// A claim for another project than the latest placement's — the session resumed in
// another bound repository — starts a new placement, keeping the earlier ones.
//
// The read-merge-write runs under a sidecar lock: two runs of one session (a resume
// in the repository while the first still exports) each add their processes, and
// without it one run's would be lost and its records dropped as another process's.
func Write(sessionID string, c Claim, now time.Time) bool {
	if c.ProjectID == "" {
		return false
	}
	p, ok := path(sessionID)
	if !ok {
		return false
	}
	// The fast path needs no lock: a fresh claim already naming these processes.
	if prev, ok := read(p); ok && samePlace(prev, c) && subset(c.PIDs, prev.PIDs) {
		if info, err := os.Stat(p); err == nil && now.Sub(info.ModTime()) < Refresh {
			return false
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	unlock, err := flock.Lock(ctx, p+".lock")
	cancel()
	if err == nil {
		defer unlock()
	}
	now = now.UTC()
	prev, havePrev := read(p)
	placements := []Placement{}
	switch {
	case !havePrev:
	case samePlace(prev, c):
		if info, err := os.Stat(p); err == nil && now.Sub(info.ModTime()) < Refresh && subset(c.PIDs, prev.PIDs) {
			return false
		}
		c.PIDs = merge(prev.PIDs, c.PIDs)
		if c.Tool == "" {
			c.Tool = prev.Tool
		}
		placements = prev.placements()
		placements = placements[:len(placements)-1]
	default:
		placements = prev.placements()
	}
	since := now
	if havePrev && samePlace(prev, c) {
		since = prev.placements()[len(prev.placements())-1].Since
	}
	placements = append(placements, Placement{ProjectID: c.ProjectID, Tool: c.Tool, Repo: c.Repo, Worktree: c.Worktree, PIDs: c.PIDs, Since: since})
	if len(placements) > maxPlacements {
		placements = placements[len(placements)-maxPlacements:]
	}
	c.Placements = placements
	c.ClaimedAt = now
	data, err := json.Marshal(c)
	if err != nil {
		return false
	}
	return config.WriteFileAtomicNoSync(p, data, 0o600) == nil
}

// samePlace reports whether c continues prev's latest placement: the same project.
func samePlace(prev, c Claim) bool {
	return prev.ProjectID == c.ProjectID
}

func subset(a, b []int) bool {
	for _, v := range a {
		if !slices.Contains(b, v) {
			return false
		}
	}
	return true
}

// merge appends the new pids to the old ones, without repeats, keeping the newest.
func merge(old, add []int) []int {
	out := slices.Clone(old)
	for _, v := range add {
		if i := slices.Index(out, v); i >= 0 {
			out = slices.Delete(out, i, i+1)
		}
		out = append(out, v)
	}
	if len(out) > maxPIDs {
		out = out[len(out)-maxPIDs:]
	}
	return out
}

// Read returns the live claim for sessionID: one written less than TTL ago.
func Read(sessionID string, now time.Time) (Claim, bool) {
	p, ok := path(sessionID)
	if !ok {
		return Claim{}, false
	}
	info, err := os.Stat(p)
	if err != nil || now.Sub(info.ModTime()) >= TTL {
		return Claim{}, false
	}
	return read(p)
}

func read(p string) (Claim, bool) {
	data, err := os.ReadFile(p)
	if err != nil {
		return Claim{}, false
	}
	var c Claim
	if json.Unmarshal(data, &c) != nil || c.ProjectID == "" {
		return Claim{}, false
	}
	return c, true
}

// Prune removes claims older than TTL. The relay runs it; hooks never do.
func Prune(now time.Time) {
	dir, err := Dir()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(filepath.Join(dir, claimsDir))
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && now.Sub(info.ModTime()) >= TTL {
			// A claim's lock goes with it; a lock alone ages out the same way.
			_ = os.Remove(filepath.Join(dir, claimsDir, e.Name()))
		}
	}
}
