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
	// Kind, Run, and Profile mirror the latest placement.
	Kind    Kind   `json:"kind,omitempty"`
	Run     string `json:"run,omitempty"`
	Profile string `json:"profile,omitempty"`
	// Evicted is set once a placement has been dropped to keep maxPlacements; MixedRuns
	// retains tombstones for any KindMixed runs dropped from Placements ("*" on overflow).
	Evicted   bool     `json:"evicted,omitempty"`
	MixedRuns []string `json:"mixed_runs,omitempty"`
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
	KindMixed      Kind = "mixed"       // crossed organizations; drops records within its run
)

// Placement is one run of a session in one location (repository or outside git).
type Placement struct {
	ProjectID string `json:"project_id"`
	Kind      Kind   `json:"kind,omitempty"`
	Run       string `json:"run,omitempty"`
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
	return []Placement{{ProjectID: c.ProjectID, Kind: c.Kind, Run: c.Run, Profile: c.Profile, Account: c.Account, Tool: c.Tool, Repo: c.Repo, Worktree: c.Worktree,
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

// lockWait caps lock waits; on timeout, non-Mixed writes back off and KindMixed falls back
// to the lock-free .mixed sidecar so an active lock holder can never overwrite Mixed.
var lockWait = 250 * time.Millisecond

// Write merges c into sessionID's claim under dir under a sidecar lock (unlocked writers
// lose concurrent PIDs) and reports whether it wrote. A project claim requires Account;
// a kinded claim may name a location alone. On lock timeout, non-Mixed writes back off
// and KindMixed writes to .mixed. KindMixed clears ProjectID; the runs ledger is authoritative.
func Write(dir, sessionID string, c Claim, now time.Time) bool {
	if c.ProjectID == "" && c.Kind == "" {
		return false
	}
	if c.Kind == KindMixed {
		c.ProjectID = ""
	}
	if c.ProjectID != "" && c.Account == "" {
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
	// Fast path skips the lock when no sidecar marks are pending and a fresh claim matches.
	if asideMarks, _ := loadAside(p, now); len(asideMarks) == 0 {
		if prev, ok := read(p); ok && canSkipRefresh(p, prev, c, now, refresh) {
			return false
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	unlock, err := flock.Lock(ctx, p+".lock")
	cancel()
	if err != nil {
		if c.Kind == KindMixed {
			return writeMixedAside(p, c, now)
		}
		return false
	}
	defer unlock()
	now = now.UTC()
	asideMarks, asideFiles := loadAside(p, now)
	prev, havePrev := read(p)
	prev, havePrev, folded := applyAside(prev, havePrev, asideMarks)
	if havePrev && staysMixed(prev, c) {
		if c.Parent != "" && prev.Parent == "" {
			prev.Parent, folded = c.Parent, true
		}
		if folded || len(asideMarks) > 0 {
			_ = persist(p, prev, asideFiles)
		} else {
			removeAside(p, asideFiles)
		}
		return false
	}
	tombForced := havePrev && tombstoneFailClosed(prev.MixedRuns, c)
	if tombForced {
		c.Kind, c.ProjectID, c.PIDs = KindMixed, "", nil
	}
	// Earlier-run Mixed writes fold into that run's slot; an unseen run may be a new run
	// starting Mixed, so falling through to append at the tail fails closed intentionally.
	if !tombForced && havePrev && c.Kind == KindMixed && c.Run != "" && prev.Run != "" && c.Run != prev.Run {
		c.ClaimedAt = now
		if matched, ch := foldEarlierRun(&prev, c); matched {
			if c.Parent != "" && prev.Parent == "" {
				prev.Parent, ch = c.Parent, true
			}
			if ch || folded || len(asideMarks) > 0 {
				return persist(p, prev, asideFiles)
			}
			removeAside(p, asideFiles)
			return false
		}
	}
	if havePrev && c.Parent == "" {
		c.Parent = prev.Parent
	}
	placements := []Placement{}
	keepSince := false
	switch {
	case !havePrev:
	case samePlace(prev, c):
		prevPls := prev.placements()
		lowerSince := c.Kind == KindMixed && now.Before(prevPls[len(prevPls)-1].Since)
		if !folded && canSkipRefresh(p, prev, c, now, refresh) {
			removeAside(p, asideFiles)
			return false
		}
		if prev.Kind == KindMixed {
			c.PIDs = unionPIDs(prev.PIDs, c.PIDs)
		} else {
			c.PIDs = merge(prev.PIDs, c.PIDs)
		}
		c.Tool, c.Cwd, c.Run = cmp.Or(c.Tool, prev.Tool), cmp.Or(c.Cwd, prev.Cwd), cmp.Or(c.Run, prev.Run)
		placements = slices.Clone(prevPls[:len(prevPls)-1])
		keepSince = !lowerSince
		if lowerSince {
			placements, c.PIDs, _ = prepareMixed(placements, c.Run, c.Profile, c.Account, c.PIDs, now)
		}
	default:
		placements = slices.Clone(prev.placements())
		if c.Kind == KindMixed && (c.Run == "" || prev.Run == "" || c.Run == prev.Run) {
			placements, c.PIDs, _ = prepareMixed(placements, c.Run, c.Profile, c.Account, c.PIDs, now)
		}
	}
	since := now
	if keepSince {
		since = prev.placements()[len(prev.placements())-1].Since
	} else if c.Kind == KindMixed {
		since = clampSince(placements, c.Run, now)
	}
	placements = append(placements, Placement{ProjectID: c.ProjectID, Kind: c.Kind, Run: c.Run, Profile: c.Profile, Account: c.Account, Tool: c.Tool, Repo: c.Repo, Worktree: c.Worktree,
		Repository: c.Repository, Root: c.Root, Cwd: c.Cwd, Remote: c.Remote, Branch: c.Branch, PIDs: c.PIDs, Since: since})
	c.Placements, c.Evicted, c.MixedRuns = trimPlacements(placements, havePrev && prev.Evicted, prev.MixedRuns)
	if tombstoneFailClosed(c.MixedRuns, c) {
		c.Kind, c.ProjectID, c.PIDs = KindMixed, "", nil
		last := &c.Placements[len(c.Placements)-1]
		last.Kind, last.ProjectID, last.PIDs = KindMixed, "", nil
		poisonRun(c.Placements, c.Run, last.Since)
	}
	c.ClaimedAt = now
	return persist(p, c, asideFiles)
}

func tombstoneFailClosed(mixed []string, c Claim) bool {
	return c.Kind != KindMixed && (c.Kind != "" || c.ProjectID != "") &&
		(slices.Contains(mixed, "*") || c.Run == "" && len(mixed) > 0)
}

func canSkipRefresh(p string, prev, c Claim, now time.Time, refresh time.Duration) bool {
	if !samePlace(prev, c) || !sameDirs(prev, c) || !sameParent(prev, c) || !subset(c.PIDs, prev.PIDs) || !sameRun(prev, c) || tombstoneFailClosed(prev.MixedRuns, c) {
		return false
	}
	if c.Kind == KindMixed && (!coveredPIDs(c.PIDs, prev.PIDs) || now.Before(prev.placements()[len(prev.placements())-1].Since)) {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && now.Sub(info.ModTime()) < refresh
}

// persist saves c atomically before removing folded sidecar marks so Readers never miss Mixed.
func persist(p string, c Claim, asideFiles []string) bool {
	data, err := json.Marshal(c)
	if err != nil || config.WriteFileAtomicNoSync(p, data, 0o600) != nil {
		return false
	}
	removeAside(p, asideFiles)
	return true
}

// samePlace reports whether c continues prev's latest run under the same sign-in.
func samePlace(prev, c Claim) bool {
	if prev.Kind != c.Kind || prev.ProjectID != c.ProjectID || prev.Repository != c.Repository || prev.Profile != c.Profile || prev.Account != c.Account {
		return false
	}
	if prev.Run != "" && c.Run != "" && prev.Run != c.Run {
		return false
	}
	return c.Kind != KindOutsideGit || prev.Cwd == c.Cwd
}

func sameRun(prev, c Claim) bool { return c.Run == "" || c.Run == prev.Run }

// staysMixed blocks non-Mixed writes to a Mixed tail and collected/kinded writes to earlier or evicted named Mixed runs.
func staysMixed(prev, c Claim) bool {
	return c.Kind != KindMixed && (prev.Kind == KindMixed && (c.Run == "" || c.Run == prev.Run) ||
		(c.Kind != "" || c.ProjectID != "") && c.Run != "" && (slices.Contains(prev.MixedRuns, c.Run) ||
			slices.ContainsFunc(prev.placements(), func(p Placement) bool { return p.Kind == KindMixed && p.Run == c.Run })))
}

// sameDirs reports whether c names the same working tree, directory, worktree, remote, and branch as prev.
func sameDirs(prev, c Claim) bool {
	return prev.Root == c.Root && (c.Cwd == "" || prev.Cwd == c.Cwd) &&
		prev.Worktree == c.Worktree && prev.Remote == c.Remote && prev.Branch == c.Branch
}

// sameParent reports whether c names the parent prev does, or none, which keeps prev's.
func sameParent(prev, c Claim) bool {
	return c.Parent == "" || c.Parent == prev.Parent
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

// Read returns sessionID's claim under the state directory dir if written less than TTL ago.
// It inspects the .mixed sidecar before reading the claim file so a concurrent fold
// (which saves the claim file before removing sidecar entries) never hides a Mixed mark.
func Read(dir, sessionID string, now time.Time) (Claim, bool) {
	p, ok := path(dir, sessionID)
	if !ok {
		return Claim{}, false
	}
	marks, _ := loadAside(p, now)
	var c Claim
	var have bool
	if info, err := os.Stat(p); err == nil && now.Sub(info.ModTime()) < TTL {
		c, have = read(p)
	}
	merged, ok, _ := applyAside(c, have, marks)
	return merged, ok
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
		if e.IsDir() && strings.HasSuffix(path, asideExt) {
			pruneAside(path, now)
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
