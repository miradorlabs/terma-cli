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
	// Kind is the latest placement's (Placement.Kind).
	Kind Kind `json:"kind,omitempty"`
	// Run is the latest placement's (Placement.Run).
	Run string `json:"run,omitempty"`
	// Profile is the latest placement's (Placement.Profile).
	Profile  string `json:"profile,omitempty"`
	Tool     string `json:"tool,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Worktree string `json:"worktree,omitempty"`
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
	// Evicted is set once a placement has been dropped to keep maxPlacements, after which
	// a record older than every kept placement is no longer known to be theirs (At).
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

// Placement kinds. A placement with no kind is a legacy one, as written before kinds:
// with a project it is a run in a repository the team collects, and with none it was
// written by Mark and means "not collected" everywhere, never "outside git".
const (
	// KindRepository is a run in a git checkout, located by its Repository whether or not
	// anything collects it.
	KindRepository Kind = "repository"
	// KindOutsideGit is a run in a directory outside any checkout, located by its Cwd.
	KindOutsideGit Kind = "outside_git"
	// KindMixed is a run that has crossed organizations: it only ever turns on within a
	// run, and its records are dropped.
	KindMixed Kind = "mixed"
)

// Placement is one run of a session in one location: a repository, or a directory
// outside git.
type Placement struct {
	ProjectID string `json:"project_id"`
	// Kind is what the location is; "" for a legacy placement. A placement with a kind
	// may name no project: a location alone, for routing to decide. Readers that predate
	// kind-aware routing treat an empty ProjectID as not collected (failing closed), so
	// hooks only emit location-only placements once the relay routes by Kind.
	Kind Kind `json:"kind,omitempty"`
	// Run is the session run the placement belongs to (internal/runs); "" when the hook
	// named none.
	Run string `json:"run,omitempty"`
	// Profile is the profile whose routing state the hook placed it under, on a machine
	// routing by teams: the relay routes its records by that profile's. "" elsewhere.
	Profile string `json:"profile,omitempty"`
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

// lockWait caps the wait for the claim's lock; if the lock cannot be had, non-Mixed
// writes back off and KindMixed writes record into the lock-free .mixed sidecar so an
// active non-Mixed lock holder can never overwrite a Mixed transition. A variable so a
// loaded machine cannot decide a test.
var lockWait = 250 * time.Millisecond

// Write merges c into sessionID's claim under the state directory dir, unless a fresh one already names its processes,
// and reports whether it wrote; a hook never fails for want of a claim. The merge runs
// under a sidecar lock: unlocked, 14 of 16 concurrent writers' processes were lost.
// c names a project (which requires an Account), or, with a Kind, may name a location alone.
// Once the latest placement is Mixed, a write of another kind replaces it only for a new run,
// and never when the lock could not be had. KindMixed clears ProjectID so a Mixed claim can
// never route to a project; the runs ledger remains authoritative for Mixed state.
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

// Mark records that sessionID runs, under processes pids, where its team does not collect,
// so the relay drops what those processes send from now on instead of holding it. The
// placement names no project and no repository; earlier placements keep their records.
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
	// The fast path needs no lock when no sidecar marks are pending and a fresh claim
	// already names these processes.
	if asideMarks, _ := loadAside(p, now); len(asideMarks) == 0 {
		if prev, ok := read(p); ok && samePlace(prev, c) && sameDirs(prev, c) && sameParent(prev, c) && subset(c.PIDs, prev.PIDs) && sameRun(prev, c) {
			if info, err := os.Stat(p); err == nil && now.Sub(info.ModTime()) < refresh {
				return false
			}
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
			// Record in the lock-free sidecar so an active lock holder cannot overwrite it.
			return writeMixedAside(p, c, now)
		}
		return false
	}
	defer unlock()
	now = now.UTC()
	asideMarks, asideFiles := loadAside(p, now)
	prev, havePrev := read(p)
	prev, havePrev, folded := applyAside(prev, havePrev, asideMarks)
	if havePrev && staysMixed(prev, c, true) {
		if folded {
			if data, err := json.Marshal(prev); err == nil && config.WriteFileAtomicNoSync(p, data, 0o600) == nil {
				removeAside(p, asideFiles)
			}
		} else if len(asideFiles) > 0 {
			removeAside(p, asideFiles)
		}
		return false
	}
	if havePrev && c.Parent == "" {
		c.Parent = prev.Parent
	}
	placements := []Placement{}
	switch {
	case !havePrev:
	case samePlace(prev, c):
		if !folded {
			if info, err := os.Stat(p); err == nil && now.Sub(info.ModTime()) < refresh && sameDirs(prev, c) && sameParent(prev, c) && subset(c.PIDs, prev.PIDs) && sameRun(prev, c) {
				if len(asideFiles) > 0 {
					removeAside(p, asideFiles)
				}
				return false
			}
		}
		c.PIDs = merge(prev.PIDs, c.PIDs)
		if c.Tool == "" {
			c.Tool = prev.Tool
		}
		if c.Cwd == "" {
			c.Cwd = prev.Cwd
		}
		if c.Run == "" {
			c.Run = prev.Run
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
	placements = append(placements, Placement{ProjectID: c.ProjectID, Kind: c.Kind, Run: c.Run, Profile: c.Profile, Account: c.Account, Tool: c.Tool, Repo: c.Repo, Worktree: c.Worktree,
		Repository: c.Repository, Root: c.Root, Cwd: c.Cwd, Remote: c.Remote, Branch: c.Branch, PIDs: c.PIDs, Since: since})
	c.Evicted = havePrev && prev.Evicted
	if len(placements) > maxPlacements {
		placements = placements[len(placements)-maxPlacements:]
		c.Evicted = true
	}
	c.Placements = placements
	c.ClaimedAt = now
	data, err := json.Marshal(c)
	if err != nil {
		return false
	}
	if config.WriteFileAtomicNoSync(p, data, 0o600) != nil {
		return false
	}
	if len(asideFiles) > 0 {
		removeAside(p, asideFiles)
	}
	return true
}

// samePlace reports whether c continues prev's latest run, under the same sign-in and
// profile. Another working tree of the same repository, or another directory in it, is
// the same run, whose Root and Cwd are rewritten in place: a placement decides where
// records go, and a session switching between worktrees must never push an earlier one
// out. A resume naming another run, or another directory outside git, is another place.
func samePlace(prev, c Claim) bool {
	if prev.Kind != c.Kind || prev.ProjectID != c.ProjectID || prev.Repository != c.Repository || prev.Profile != c.Profile || prev.Account != c.Account {
		return false
	}
	if prev.Run != "" && c.Run != "" && prev.Run != c.Run {
		return false
	}
	return c.Kind != KindOutsideGit || prev.Cwd == c.Cwd
}

// sameRun reports whether c names no run or prev's, so a fresh claim needs no rewrite.
func sameRun(prev, c Claim) bool {
	return c.Run == "" || c.Run == prev.Run
}

// staysMixed reports whether c must not replace prev's latest placement because it is
// Mixed: Mixed only turns on within a run. Only a write naming another run, made under
// the lock, may replace it. A write naming no run cannot tell whether its run is new, and
// one made without the lock could undo a crossing a concurrent hook is recording.
func staysMixed(prev, c Claim, locked bool) bool {
	if prev.Kind != KindMixed || c.Kind == KindMixed {
		return false
	}
	return !locked || c.Run == "" || c.Run == prev.Run
}

// sameDirs reports whether c names the working tree, directory, worktree, remote and branch
// prev does, so a fresh claim needs no rewrite: a checkout of another branch in the same tree
// is told at the next hook. A claim naming no directory (a git hook's) keeps prev's.
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
		p := filepath.Join(claims, e.Name())
		if e.IsDir() && strings.HasSuffix(p, asideExt) {
			pruneAside(p, now)
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < TTL {
			continue
		}
		if !strings.HasSuffix(p, ".lock") {
			_ = os.Remove(p)
		} else if unlock, err := flock.TryLock(p); err == nil {
			// A lock's time is its creation, so an old one may still be in use.
			flock.Remove(p, unlock)
		}
	}
}
