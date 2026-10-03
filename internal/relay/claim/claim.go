// Package claim is how hooks tell the local relay which sessions belong to an opted-in
// repository: one small file per session. It has no OTLP dependency because every hook
// imports it and the hook path has a latency budget.
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

// DirName is the relay's directory under the config dir.
const DirName = "relay"

const (
	claimsDir = "claims"
	tokenFile = "token"
)

// TTL is how long a claim stays valid after its last write; it matches the hooks' ActiveTTL.
const TTL = 4 * time.Hour

// Refresh is how old a claim may be before a hook rewrites it, so hundreds of tool calls cost
// one write every few minutes.
const Refresh = 5 * time.Minute

// Claim says which project a session's telemetry belongs to; its top-level fields are the latest placement.
type Claim struct {
	ProjectID string `json:"project_id"`
	Tool      string `json:"tool,omitempty"`
	Repo      string `json:"repo,omitempty"`
	Worktree  string `json:"worktree,omitempty"`
	// Repository is the working copy as admission names it; the relay rechecks it.
	Repository config.Repository `json:"repository,omitzero"`
	ClaimedAt  time.Time         `json:"claimed_at"`
	// PIDs are the processes the claiming hooks ran under, so a session resumed by another
	// process where no hook runs is not covered. Empty matches any sender.
	PIDs []int `json:"pids,omitempty"`
	// Placements are where the session has run, oldest first: an agent can keep a session id
	// across resumes in another repository, and the first run's late records stay its own.
	Placements []Placement `json:"placements,omitempty"`
}

// Placement is one run of a session in one repository.
type Placement struct {
	ProjectID string `json:"project_id"`
	Tool      string `json:"tool,omitempty"`
	Repo      string `json:"repo,omitempty"`
	Worktree  string `json:"worktree,omitempty"`
	// Repository is Claim.Repository for this run.
	Repository config.Repository `json:"repository,omitzero"`
	PIDs       []int             `json:"pids,omitempty"`
	Since      time.Time         `json:"since"`
}

// maxPIDs allows a few runs of one session, each with its chain of ancestors.
const maxPIDs = 64

const maxPlacements = 8

// Covers reports whether pid may send under this claim's latest placement.
func (c Claim) Covers(pid int) bool {
	return covers(c.PIDs, pid)
}

// covers never admits an unidentified sender (pid 0) to a placement that names
// processes, which would let a session resumed elsewhere through.
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
	return []Placement{{ProjectID: c.ProjectID, Tool: c.Tool, Repo: c.Repo, Worktree: c.Worktree, Repository: c.Repository, PIDs: c.PIDs}}
}

// At is the claim as it applies to a record pid sent at time at: the covering placement
// latest to start by at. False when no placement covers pid.
func (c Claim) At(pid int, at time.Time) (Claim, bool) {
	var best *Placement
	all := c.placements()
	for i := range all {
		p := &all[i]
		if !covers(p.PIDs, pid) {
			continue
		}
		if best == nil || at.IsZero() || !p.Since.After(at) {
			best = p
		}
	}
	if best == nil {
		return Claim{}, false
	}
	return Claim{ProjectID: best.ProjectID, Tool: best.Tool, Repo: best.Repo, Worktree: best.Worktree,
		Repository: best.Repository, PIDs: best.PIDs, ClaimedAt: c.ClaimedAt, Placements: c.Placements}, true
}

// Dir is the relay directory, under the config dir.
func Dir() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, DirName), nil
}

// Enabled reports whether `terma relay setup` wrote the relay's token; without it hooks write no claims.
func Enabled() bool {
	dir, err := Dir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(dir, tokenFile))
	return err == nil
}

// DefaultAddr is where the relay listens; it is fixed because exporter configuration is static.
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

// lockWait caps the wait for the claim's lock; a lock that cannot be had costs the write its
// exclusivity, never the write itself. A variable so a loaded machine cannot decide a test.
var lockWait = 250 * time.Millisecond

// Write merges c into sessionID's claim, unless a fresh one already names its processes,
// and reports whether it wrote; a hook never fails for want of a claim. The merge runs
// under a sidecar lock: unlocked, 14 of 16 concurrent writers' processes were lost.
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
	placements = append(placements, Placement{ProjectID: c.ProjectID, Tool: c.Tool, Repo: c.Repo, Worktree: c.Worktree, Repository: c.Repository, PIDs: c.PIDs, Since: since})
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

func samePlace(prev, c Claim) bool {
	return prev.ProjectID == c.ProjectID && prev.Repository.Equal(c.Repository)
}

func subset(a, b []int) bool {
	for _, v := range a {
		if !slices.Contains(b, v) {
			return false
		}
	}
	return true
}

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

// Read returns sessionID's claim if written less than TTL ago.
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
			_ = os.Remove(filepath.Join(dir, claimsDir, e.Name()))
		}
	}
}
