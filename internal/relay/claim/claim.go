// Package claim is how hooks tell the local relay which sessions belong to an opted-in
// repository: one small file per session. It has no OTLP dependency because every hook
// imports it and the hook path has a latency budget.
package claim

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// DirName is the relay's directory under the state directory.
const DirName = "relay"

const (
	claimsDir = "claims"
	tokenFile = "token"
)

// TTL is how long a claim stays valid after its last write.
const TTL = 4 * time.Hour

// Refresh is how old a claim may be before a hook rewrites it, so hundreds of tool calls cost
// one write every few minutes.
const Refresh = 5 * time.Minute

// markRefresh is Refresh for a session marked not collected, which only needs to outlive
// its TTL: one write per session.
const markRefresh = TTL / 2

// Claim says which project a session's telemetry belongs to; its top-level fields are the latest placement.
// An empty ProjectID with no Kind is a run in a working copy the team does not collect (Mark).
type Claim struct {
	ProjectID string `json:"project_id"`
	Tool      string `json:"tool,omitempty"`
	Repo      string `json:"repo,omitempty"`
	Worktree  string `json:"worktree,omitempty"`
	// Repository is the working copy as admission names it; the relay rechecks it.
	Repository config.Repository `json:"repository,omitzero"`
	// Root is the working tree the claiming hook ran in, "" outside git: what the relay
	// stamps as terma.repository.root.
	Root string `json:"root,omitempty"`
	// Cwd is the directory the claiming agent hook ran in, symlink-resolved: what the relay
	// stamps as terma.working_directory. A git hook names none and keeps the session's.
	Cwd string `json:"cwd,omitempty"`
	// Remote is the working copy's origin, normalised without credentials, and Branch the
	// branch its HEAD is on, "" when detached: what the relay stamps as
	// vcs.repository.url.full and vcs.ref.head.name.
	Remote    string    `json:"remote,omitempty"`
	Branch    string    `json:"branch,omitempty"`
	ClaimedAt time.Time `json:"claimed_at"`
	// Parent is the session that opened this one for a subagent, as its start hook named it:
	// what the relay stamps as terma.session.parent.id. It is the session's, so a later hook
	// that names none keeps it.
	Parent string `json:"parent,omitempty"`
	// PIDs are the processes the claiming hooks ran under, so a session resumed by another
	// process where no hook runs is not covered. Empty matches any sender.
	PIDs []int `json:"pids,omitempty"`
	// Placements are where the session has run, oldest first: an agent can keep a session id
	// across resumes in another repository, and the first run's late records stay its own.
	Placements []Placement `json:"placements,omitempty"`
	// Kind and Profile mirror the latest placement.
	Kind    Kind   `json:"kind,omitempty"`
	Profile string `json:"profile,omitempty"`
	// Evicted is set once a placement has been dropped to keep maxPlacements.
	Evicted bool `json:"evicted,omitempty"`
	// Account is the lane of the sign-in the claiming hook ran under: its account key, or
	// sendas.NoPersonLane under a server key. The team is the one chosen under it, so the
	// relay resolves the claim only for that sign-in.
	Account string `json:"account,omitempty"`
	// CatchAll marks a claim the relay made from the profile's own policy (global mode's
	// catch-all), which no hook made under a sign-in; never stored.
	CatchAll bool `json:"-"`
}

// Kind is what a placement's location is.
type Kind string

// Placement kinds ("" is legacy: with a project = collected, without = Mark exclusion).
const (
	KindRepository Kind = "repository"  // git checkout located by Repository
	KindOutsideGit Kind = "outside_git" // directory outside git located by Cwd
)

// Placement is one run of a session in one location (repository or outside git).
type Placement struct {
	ProjectID string `json:"project_id"`
	Kind      Kind   `json:"kind,omitempty"`
	Profile   string `json:"profile,omitempty"`
	// Account is Claim.Account for this run.
	Account  string `json:"account,omitempty"`
	Tool     string `json:"tool,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	// Repository is Claim.Repository for this run.
	Repository config.Repository `json:"repository,omitzero"`
	// Root is Claim.Root for this run.
	Root string `json:"root,omitempty"`
	// Cwd is Claim.Cwd for this run.
	Cwd string `json:"cwd,omitempty"`
	// Remote and Branch are Claim.Remote and Claim.Branch for this run.
	Remote string    `json:"remote,omitempty"`
	Branch string    `json:"branch,omitempty"`
	PIDs   []int     `json:"pids,omitempty"`
	Since  time.Time `json:"since"`
}

// maxPIDs allows a few runs of one session, each with its chain of ancestors.
const maxPIDs = 64

const maxPlacements = 8

// Covers reports whether pid may send under this claim's latest placement.
func (c Claim) Covers(pid int) bool {
	return covers(c.PIDs, pid)
}

// CoversPlace reports whether c's latest placement already covers want's location, working
// tree, parent and PIDs, matching when Write skips a redundant rewrite.
func (c Claim) CoversPlace(want Claim) bool {
	return samePlace(c, want) && sameDirs(c, want) && sameParent(c, want) && subset(want.PIDs, c.PIDs)
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
	return []Placement{{ProjectID: c.ProjectID, Kind: c.Kind, Profile: c.Profile, Account: c.Account, Tool: c.Tool, Repo: c.Repo, Worktree: c.Worktree,
		Repository: c.Repository, Root: c.Root, Cwd: c.Cwd, Remote: c.Remote, Branch: c.Branch, PIDs: c.PIDs}}
}

// Dir is the relay directory under the state directory dir.
func Dir(dir string) string { return filepath.Join(dir, DirName) }

// Enabled reports whether `terma relay setup` wrote the relay's token under the state
// directory dir; without it hooks write no claims.
func Enabled(dir string) bool {
	_, err := os.Stat(TokenPath(dir))
	return err == nil
}

// DefaultAddr is where the relay listens; it is fixed because exporter configuration is static.
const DefaultAddr = "127.0.0.1:43180"

// TokenPath is where the relay's local token lives under the state directory dir.
func TokenPath(dir string) string { return filepath.Join(Dir(dir), tokenFile) }

func path(dir, sessionID string) (string, bool) {
	if !session.ValidID(sessionID) {
		return "", false
	}
	return filepath.Join(Dir(dir), claimsDir, sessionID+".json"), true
}

// lockWait caps lock waits; on timeout, writes back off rather than blocking the hook.
var lockWait = 250 * time.Millisecond

// Write merges c into sessionID's claim under dir under a sidecar lock (unlocked writers
// lose concurrent PIDs) and reports whether it wrote. A project claim requires Account;
// a kinded claim may name a location alone.
func Write(dir, sessionID string, c Claim, now time.Time) bool {
	if c.ProjectID == "" && c.Kind == "" || c.ProjectID != "" && c.Account == "" {
		return false
	}
	return write(dir, sessionID, c, now)
}

// Mark records that sessionID runs under pids where its team does not collect so the relay
// drops those processes' records instead of holding them.
func Mark(dir, sessionID, tool string, pids []int, now time.Time) bool {
	return write(dir, sessionID, Claim{Tool: tool, PIDs: pids}, now)
}

func write(dir, sessionID string, c Claim, now time.Time) bool {
	refresh := Refresh
	if c.ProjectID == "" && c.Kind == "" {
		refresh = markRefresh
	}
	p, ok := path(dir, sessionID)
	if !ok {
		return false
	}
	if prev, ok := read(p); ok && canSkipRefresh(p, prev, c, now, refresh) {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	unlock, err := flock.Lock(ctx, p+".lock")
	cancel()
	if err != nil {
		return false
	}
	defer unlock()
	now = now.UTC()
	prev, havePrev := read(p)
	if havePrev && c.Parent == "" {
		c.Parent = prev.Parent
	}
	var placements []Placement
	keepSince := false
	switch {
	case !havePrev:
	case samePlace(prev, c):
		if canSkipRefresh(p, prev, c, now, refresh) {
			return false
		}
		prevPls := prev.placements()
		c.PIDs = merge(prev.PIDs, c.PIDs)
		c.Tool, c.Cwd = cmp.Or(c.Tool, prev.Tool), cmp.Or(c.Cwd, prev.Cwd)
		placements = slices.Clone(prevPls[:len(prevPls)-1])
		keepSince = true
	default:
		placements = slices.Clone(prev.placements())
	}
	since := now
	if keepSince {
		since = prev.placements()[len(prev.placements())-1].Since
	}
	placements = append(placements, Placement{ProjectID: c.ProjectID, Kind: c.Kind, Profile: c.Profile, Account: c.Account, Tool: c.Tool, Repo: c.Repo, Worktree: c.Worktree,
		Repository: c.Repository, Root: c.Root, Cwd: c.Cwd, Remote: c.Remote, Branch: c.Branch, PIDs: c.PIDs, Since: since})
	c.Placements, c.Evicted = trimPlacements(placements, havePrev && prev.Evicted)
	c.ClaimedAt = now
	data, err := json.Marshal(c)
	return err == nil && config.WriteFileAtomicNoSync(p, data, 0o600) == nil
}

func trimPlacements(pls []Placement, evicted bool) ([]Placement, bool) {
	if len(pls) <= maxPlacements {
		return pls, evicted
	}
	return pls[len(pls)-maxPlacements:], true
}

func canSkipRefresh(p string, prev, c Claim, now time.Time, refresh time.Duration) bool {
	if !samePlace(prev, c) || !sameDirs(prev, c) || !sameParent(prev, c) || !subset(c.PIDs, prev.PIDs) {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && now.Sub(info.ModTime()) < refresh
}

func samePlace(prev, c Claim) bool {
	return prev.Kind == c.Kind && prev.ProjectID == c.ProjectID && prev.Repository == c.Repository && prev.Profile == c.Profile && prev.Account == c.Account &&
		(c.Kind != KindOutsideGit || prev.Cwd == c.Cwd)
}
func sameDirs(prev, c Claim) bool {
	return prev.Root == c.Root && (c.Cwd == "" || prev.Cwd == c.Cwd) && prev.Worktree == c.Worktree && prev.Remote == c.Remote && prev.Branch == c.Branch
}
func sameParent(prev, c Claim) bool { return c.Parent == "" || c.Parent == prev.Parent }
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

// Read returns sessionID's claim under the state directory dir if written less than TTL ago.
func Read(dir, sessionID string, now time.Time) (Claim, bool) {
	p, ok := path(dir, sessionID)
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
	if json.Unmarshal(data, &c) != nil || c.ProjectID == "" && len(c.Placements) == 0 {
		return Claim{}, false
	}
	return c, true
}

// Prune removes claims under the state directory dir older than TTL. The relay runs it;
// hooks never do.
func Prune(dir string, now time.Time) {
	claims := filepath.Join(Dir(dir), claimsDir)
	entries, err := os.ReadDir(claims)
	if err != nil {
		return
	}
	for _, e := range entries {
		path := filepath.Join(claims, e.Name())
		// Clean up legacy .mixed sidecar directories left by earlier builds.
		if e.IsDir() && strings.HasSuffix(path, ".mixed") {
			pruneLegacyMixedDir(path, now)
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < TTL {
			continue
		}
		if !strings.HasSuffix(path, ".lock") {
			_ = os.Remove(path)
		} else if unlock, err := flock.TryLock(path); err == nil {
			// A lock's time is its creation, so an old one may still be in use.
			flock.Remove(path, unlock)
		}
	}
}

func pruneLegacyMixedDir(dir string, now time.Time) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) >= TTL {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	_ = os.Remove(dir)
}
