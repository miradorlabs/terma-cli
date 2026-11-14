package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// A read, a search or a shell command in another organization's checkout carries its
// content into the session's telemetry, so what a call reached counts toward a crossing,
// not only what it edited. Such calls only classify: they record and send nothing else.

// Classify classifies, on a migrated machine, the checkouts that paths (files or folders,
// absolute or relative to Cwd) reach, a symlink's target's as well as its own, with the
// session's location r and the run's earlier locations (classifyTouches); UnknownReach among
// paths turns the run Mixed, the ledger naming the command it follows. It writes no manifest and spools nothing. A nil r is located
// here, with no store and nothing unparked: for a hook before its tool call runs, or in a
// location Route drops, so a later hook still sees the crossing. It returns r, or the
// checkout it located, for RepoAt; nil for none.
func (e Env) Classify(ctx context.Context, r *Repo, sess session.Session, paths []string, extra map[string]any) *Repo {
	var unknown runs.Why
	paths = slices.DeleteFunc(slices.Clone(paths), func(p string) bool {
		if isUnknownReach(p) {
			_, command, _ := strings.Cut(p[len(UnknownReach):], "\x00")
			unknown = runs.Why{Reason: runs.UnknownReach, Where: command}
		}
		return p == "" || isUnknownReach(p)
	})
	if e.Routing == nil || !e.Routing.Usable() || len(paths) == 0 && unknown.Reason == "" || !session.ValidID(sess.ID) {
		return r
	}
	if r == nil {
		var err error
		if r, err = e.routeHere(ctx); err != nil {
			e.classifyUnlocated(sess, extra, e.reached(nil, paths), unknown)
			return nil
		}
	}
	e.classifyTouches(r, sess, extra, e.reached(r, paths), unknown)
	return r
}

// classifyUnlocated is classifyTouches for a hook whose own checkout cannot be located (its
// cwd deleted, say): what the call reached is classified against where the run has been.
// It writes a placement only if the run turns Mixed.
//
// Accepted risk: the hook's own location is not classified. Marking the run Mixed for it
// would turn every session whose cwd was deleted Mixed. The run's earlier hooks recorded
// where it ran, and the next hook that can locate its checkout classifies it.
func (e Env) classifyUnlocated(sess session.Session, extra map[string]any, reached []touched, unknown runs.Why) {
	var locs []routing.Location
	for _, t := range reached {
		if t.repo.dest != nil {
			locs = append(locs, t.repo.dest.loc)
		}
	}
	if len(locs) == 0 && unknown.Reason == "" {
		return
	}
	agent, _ := extra[semconv.TermaAgentIDKey].(string)
	e.judge(sess.ID, agent, claim.Claim{Tool: sess.Tool, PIDs: claimPIDs()}, locs, false, unknown)
}

// reached are the checkouts other than r (which may be nil) that paths reach, each once: a
// folder's own, else its parent's, and for a path that is or passes through a symlink, its
// target's too. A path in no checkout reaches none.
func (e Env) reached(r *Repo, paths []string) []touched {
	var out []touched
	dirs := map[string]bool{}
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(e.Cwd, p)
		}
		p = filepath.Clean(p)
		at := []string{p}
		if target, err := filepath.EvalSymlinks(p); err == nil && target != p {
			at = append(at, target)
		}
		for _, p := range at {
			dir := p
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				dir = filepath.Dir(dir)
			}
			if dirs[dir] {
				continue
			}
			dirs[dir] = true
			repo := e.checkoutOf(r, dir)
			if repo == nil || repo == r || slices.ContainsFunc(out, func(t touched) bool { return samePath(t.repo.Root, repo.Root) }) {
				continue
			}
			out = append(out, touched{repo: repo})
		}
	}
	return out
}
