package history

import (
	"cmp"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// RouteTarget is the resolved destination team, location, and checkout metadata for one segment cwd.
type RouteTarget struct {
	Team     routing.Team
	Location routing.Location
	Checkout relay.CheckoutInfo
}

type cachedCwd struct {
	target     RouteTarget
	ok         bool
	skipReason string
}

// cwdResolver resolves each segment's recorded working directory through the same
// repository detection and routing.Router rules live hooks use, caching per-cwd lookups
// so a large transcript only inspects each directory once.
type cwdResolver struct {
	state            routing.State
	router           *routing.Router
	authURL          string
	serverKey        bool
	serverKeyTeam    string
	serverKeyAuthURL string
	now              time.Time
	cache            map[string]cachedCwd
}

func newCwdResolver(st routing.State, authURL string, serverKey bool, serverKeyTeam, serverKeyAuthURL string, now time.Time) *cwdResolver {
	return &cwdResolver{
		state:            st,
		router:           routing.NewRouter(st),
		authURL:          authURL,
		serverKey:        serverKey,
		serverKeyTeam:    serverKeyTeam,
		serverKeyAuthURL: serverKeyAuthURL,
		now:              now,
		cache:            map[string]cachedCwd{},
	}
}

// resolve maps (cwd, fallbackRepoURL, fallbackBranch) to a destination RouteTarget or a
// skip reason. Empty cwd always skips with "no_cwd". A missing directory on disk fails
// closed with "missing_cwd" unless the transcript itself recorded a hosted git origin for
// that directory, so a deleted work repository can never fall back to personal.
func (r *cwdResolver) resolve(ctx context.Context, cwd, fallbackRepoURL, fallbackBranch string) (RouteTarget, bool, string) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return RouteTarget{}, false, SkipNoCwd
	}
	cacheKey := cwd + "\x00" + fallbackRepoURL + "\x00" + fallbackBranch
	if hit, ok := r.cache[cacheKey]; ok {
		return hit.target, hit.ok, hit.skipReason
	}
	target, ok, reason := r.resolveUncached(ctx, cwd, fallbackRepoURL, fallbackBranch)
	r.cache[cacheKey] = cachedCwd{target: target, ok: ok, skipReason: reason}
	return target, ok, reason
}

func (r *cwdResolver) resolveUncached(ctx context.Context, cwd, fallbackRepoURL, fallbackBranch string) (RouteTarget, bool, string) {
	loc, checkout, skipReason := locateSegmentCheckout(ctx, cwd, fallbackRepoURL, fallbackBranch)
	if skipReason != "" {
		return RouteTarget{}, false, skipReason
	}
	baseTarget := RouteTarget{Location: loc, Checkout: checkout}
	d := r.router.Route(loc, r.now)
	switch d.Outcome {
	case routing.Send:
		team, ok := r.state.Team(d.EnrollmentID)
		if !ok {
			return baseTarget, false, SkipNotCollected
		}
		if team.Policy.CollectsNothing || team.Policy.Unset {
			return baseTarget, false, SkipNotCollected
		}
		if r.authURL != "" && team.AuthURL != "" && !sameHost(team.AuthURL, r.authURL) {
			return baseTarget, false, SkipWrongEnvironment
		}
		if r.serverKey {
			if r.authURL != "" && !sameHost(r.serverKeyAuthURL, r.authURL) {
				return baseTarget, false, SkipWrongEnvironment
			}
			if r.serverKeyTeam == "" || team.TeamID != r.serverKeyTeam {
				return baseTarget, false, SkipNotCollected
			}
		}
		return RouteTarget{Team: team, Location: loc, Checkout: checkout}, true, ""
	case routing.Hold:
		if d.Blocked != "" {
			return baseTarget, false, SkipTeamBlockedPrefix + string(d.Blocked)
		}
		return baseTarget, false, SkipTeamBlockedPrefix + string(d.Reason)
	default:
		return baseTarget, false, SkipNotCollected
	}
}

func locateSegmentCheckout(ctx context.Context, cwd, fallbackRepoURL, fallbackBranch string) (routing.Location, relay.CheckoutInfo, string) {
	abs := filepath.Clean(cwd)
	if !filepath.IsAbs(abs) {
		return routing.Location{}, relay.CheckoutInfo{}, SkipMissingCwd
	}
	recordedOrigin := ""
	if fallbackRepoURL != "" {
		if id := gitx.RepositoryID(fallbackRepoURL); routing.Owner(id) != "" {
			recordedOrigin = id
		}
	}
	info, statErr := os.Stat(abs)
	if statErr != nil || !info.IsDir() {
		// Deleted or moved working directory: never climb to a parent folder (which could
		// falsely classify a deleted org repo as OutsideGit -> Personal). Only route when
		// the transcript itself recorded a hosted repository URL for this directory.
		if recordedOrigin != "" {
			return routing.Location{Kind: routing.Repository, Repository: recordedOrigin}, relay.CheckoutInfo{
				Remote: gitx.NormalizeRemote(fallbackRepoURL),
				Origin: recordedOrigin,
				Branch: fallbackBranch,
				Cwd:    abs,
			}, ""
		}
		return routing.Location{}, relay.CheckoutInfo{}, SkipMissingCwd
	}

	root, gitDir, err := termaproject.Locate(ctx, abs)
	if err != nil {
		return routing.Location{}, relay.CheckoutInfo{}, SkipUnroutable
	}
	checkout := relay.CheckoutInfo{Cwd: abs}
	if gitDir != "" {
		checkout.Root = root
		origin, remoteURL := gitx.OriginFS(gitDir)
		checkout.Branch = cmp.Or(fallbackBranch, gitx.BranchFS(gitDir))
		if wtName, _, linked := gitx.LinkedWorktreeFS(gitDir); linked {
			checkout.Worktree = wtName
		}
		if routing.Owner(origin) != "" {
			// If the transcript recorded a hosted origin for this directory and disk now
			// points at a different repository, fail closed rather than sending one
			// organization's transcript to another.
			if recordedOrigin != "" && !strings.EqualFold(origin, recordedOrigin) {
				return routing.Location{}, relay.CheckoutInfo{}, SkipOriginMismatch
			}
			checkout.Origin = origin
			checkout.Remote = remoteURL
			return routing.Location{Kind: routing.Repository, Repository: origin}, checkout, ""
		}
	}
	// If the transcript recorded a hosted repository for this directory but disk is no
	// longer that repository, fail closed instead of falling back to OutsideGit/Personal.
	if recordedOrigin != "" {
		return routing.Location{}, relay.CheckoutInfo{}, SkipOriginMismatch
	}
	return routing.Location{Kind: routing.OutsideGit}, checkout, ""
}

func sameHost(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ua, ea := url.Parse(a)
	ub, eb := url.Parse(b)
	if ea != nil || eb != nil {
		return strings.EqualFold(strings.TrimRight(a, "/"), strings.TrimRight(b, "/"))
	}
	return strings.EqualFold(ua.Host, ub.Host)
}

func indexBySession(files []agents.HistoryFile) map[string]agents.HistoryFile {
	out := make(map[string]agents.HistoryFile, len(files))
	for _, f := range files {
		out[f.SessionID] = f
	}
	return out
}

func buildChildrenMap(files []agents.HistoryFile) map[string][]agents.HistoryFile {
	out := map[string][]agents.HistoryFile{}
	for _, f := range files {
		if f.ParentSessionID != "" && f.ParentSessionID != f.SessionID && !f.OneWayParent {
			out[f.ParentSessionID] = append(out[f.ParentSessionID], f)
		}
	}
	return out
}

// collectFamily resolves the spawn and ancestor tree relevant to f: walking up
// ParentSessionID through every ancestor (both bidirectional subagents and one-way
// forks/continuations), then collecting each ancestor and its bidirectional subagent tree
// except f itself. One-way forks inherit their ancestors' cutoffs without propagating back
// into the original parent. If f's ancestor chain references a parent session whose
// transcript is absent from disk, missingParent returns that parent session ID so the caller
// fails closed.
func collectFamily(
	f agents.HistoryFile,
	bySession map[string]agents.HistoryFile,
	childrenByParent map[string][]agents.HistoryFile,
) (companions []agents.HistoryFile, missingParent string) {
	queue := []agents.HistoryFile{f}
	seenUp := map[string]bool{f.SessionID: true}
	for cur := f; cur.ParentSessionID != ""; {
		pid := cur.ParentSessionID
		if seenUp[pid] {
			break
		}
		seenUp[pid] = true
		parent, ok := bySession[pid]
		if !ok {
			missingParent = pid
			break
		}
		queue = append(queue, parent)
		cur = parent
	}
	visited := map[string]bool{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur.SessionID] {
			continue
		}
		visited[cur.SessionID] = true
		if cur.SessionID != f.SessionID {
			companions = append(companions, cur)
		}
		queue = append(queue, childrenByParent[cur.SessionID]...)
	}
	return companions, missingParent
}

// familyReadError returns the first transcript or directory discovery could not read for
// f or any member of its spawn family, empty when every one was read.
func familyReadError(f agents.HistoryFile, companions []agents.HistoryFile) string {
	if f.CompanionReadError != "" {
		return f.CompanionReadError
	}
	for _, c := range companions {
		if c.CompanionReadError != "" {
			return c.CompanionReadError
		}
	}
	return ""
}

// ancestorCutoffs maps each companion session reached through one or more OneWayParent
// edges from f to the earliest fork/continuation timestamp along that upward path, so
// unrelated post-fork activity on an ancestor does not suppress an independent fork.
func ancestorCutoffs(f agents.HistoryFile, companions []agents.HistoryFile) map[string]time.Time {
	byID := make(map[string]agents.HistoryFile, len(companions)+1)
	byID[f.SessionID] = f
	bidirChildren := map[string][]string{}
	for _, c := range companions {
		byID[c.SessionID] = c
		if c.ParentSessionID != "" && c.ParentSessionID != c.SessionID && !c.OneWayParent {
			bidirChildren[c.ParentSessionID] = append(bidirChildren[c.ParentSessionID], c.SessionID)
		}
	}
	cutoffs := map[string]time.Time{}
	var running time.Time
	seen := map[string]bool{f.SessionID: true}
	for cur := f; cur.ParentSessionID != ""; {
		if cur.OneWayParent && !cur.OneWayCutoff.IsZero() {
			if running.IsZero() || cur.OneWayCutoff.Before(running) {
				running = cur.OneWayCutoff
			}
		}
		pid := cur.ParentSessionID
		if seen[pid] {
			break
		}
		seen[pid] = true
		parent, ok := byID[pid]
		if !ok {
			break
		}
		if !running.IsZero() {
			// Propagate the one-way cutoff to this ancestor and its bidirectional subagents.
			q := []string{pid}
			for len(q) > 0 {
				id := q[0]
				q = q[1:]
				if prev, exists := cutoffs[id]; !exists || running.Before(prev) {
					cutoffs[id] = running
				}
				q = append(q, bidirChildren[id]...)
			}
		}
		cur = parent
	}
	return cutoffs
}
