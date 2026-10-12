package hookrun

import (
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// The session start, end and edit steps every agent shares, so the agents cannot drift apart.

// NewSession is the record of a session seen now in r.
func (e Env) NewSession(r *Repo, id, tool, model string) session.Session {
	now := e.Time()
	return session.Session{ID: id, Tool: tool, Model: model, Cwd: r.Root, StartedAt: now, UpdatedAt: now}
}

// SetActive records sess as the checkout's active session; a failure is only logged.
func (e Env) SetActive(r *Repo, sess session.Session) {
	if err := r.Store.SetActive(sess); err != nil {
		e.Logf("record session: %v", err)
	}
}

// PruneManifests drops touched-files records older than ManifestRetention.
func (e Env) PruneManifests(r *Repo, now time.Time) {
	if _, err := r.Store.Prune(now.Add(-ManifestRetention)); err != nil {
		e.Logf("prune: %v", err)
	}
}

// EmitStart spools the session's start, with extra attributes only one agent knows.
func (e Env) EmitStart(r *Repo, sess session.Session, extra map[string]any) {
	attrs := map[string]any{semconv.GenAIMainAgentNameKey: sess.Tool}
	BoundedAttr(attrs, semconv.GenAIRequestModelKey, sess.Model)
	maps.Copy(attrs, extra)
	e.EmitFor(r, spool.Event{Name: semconv.TermaSessionStartEvent, SessionID: sess.ID, Attrs: attrs})
}

// Announce makes the session active, ages out old manifests and spools the start.
func (e Env) Announce(r *Repo, sess session.Session, extra map[string]any) {
	e.SetActive(r, sess)
	e.PruneManifests(r, sess.UpdatedAt)
	e.EmitStart(r, sess, extra)
}

// EndSession clears the active session and spools the end; manifests stay, the work may be uncommitted.
func (e Env) EndSession(r *Repo, id, tool string) {
	_ = r.Store.ClearActive(session.Key{Tool: tool, ID: id})
	e.EmitFor(r, spool.Event{Name: semconv.TermaSessionEndEvent, SessionID: id, Attrs: map[string]any{
		semconv.GenAIMainAgentNameKey: tool,
	}})
}

// Touch spools the reported paths, absolute or relative to Cwd, as one event per checkout,
// naming its repository. The session's own checkout r, and any other checkout of its
// repository, also record them in their manifests, for the commit stamping; only r claims the
// session. Another checkout is reported only if the team policy admits it, and a path in no
// admitted checkout is dropped.
func (e Env) Touch(r *Repo, sess session.Session, toolName string, paths []string, extra map[string]any) {
	e.Expect(r, sess, paths)
	e.Report(r, sess, toolName, paths, extra)
}

// Expect records in sess's manifest the files of r among paths, without reporting them, and
// returns them repo-relative: a hook before a tool call records what a commit in the call
// will take in. Another checkout of r's repository (a linked worktree, where a session started
// in the main checkout often works, or the main checkout from a worktree session) records its
// files in its own manifest too, since a commit there is stamped from it; it is not claimed.
func (e Env) Expect(r *Repo, sess session.Session, paths []string) []string {
	var own []string
	for _, c := range e.checkouts(r, paths) {
		store := r.Store
		if c.repo != r {
			if !sameRepository(r, c.repo) {
				continue
			}
			dir, err := project.StoreDir(e.StateDir, c.repo.Root, c.repo.GitDir)
			if err != nil {
				e.Logf("worktree store: %v", err)
				continue
			}
			// Only a session announced in the worktree prunes its store, so a mirrored touch expires here.
			store = session.Open(dir)
			if _, err := store.Prune(e.Time().Add(-ManifestRetention)); err != nil {
				e.Logf("worktree prune: %v", err)
			}
		}
		if err := store.Touch(sess, c.files, e.Time()); err != nil {
			e.Logf("record files: %v", err)
			continue
		}
		if c.repo == r {
			own = c.files
		}
	}
	return own
}

// Report spools paths as Touch does, without recording them in the manifest.
func (e Env) Report(r *Repo, sess session.Session, toolName string, paths []string, extra map[string]any) {
	for _, c := range e.checkouts(r, paths) {
		attrs := map[string]any{
			semconv.GenAIMainAgentNameKey: sess.Tool, semconv.GenAIToolNameKey: toolName, semconv.TermaFilesPathsKey: c.files,
		}
		maps.Copy(attrs, extra)
		if remote := c.repo.Remote; remote != "" {
			attrs[semconv.VCSRepositoryURLFullKey] = remote
		}
		vcsAttrs(attrs, c.repo)
		ev := spool.Event{Name: semconv.TermaFilesTouchedEvent, SessionID: sess.ID, Attrs: attrs}
		if c.repo == r {
			e.EmitFor(r, ev)
		} else {
			e.emit(e.stamp(c.repo, ev))
		}
	}
}

// touched is the files of one checkout, repo-relative, as a set.
type touched struct {
	repo  *Repo
	files []string
}

// checkouts groups paths by the admitted checkout each is in, in order of first mention.
func (e Env) checkouts(r *Repo, paths []string) []touched {
	var out []touched
	byDir := map[string]*Repo{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(e.Cwd, p)
		}
		dir := filepath.Dir(p)
		repo, seen := byDir[dir]
		if !seen {
			repo = e.checkoutOf(r, dir)
			byDir[dir] = repo
		}
		if repo == nil {
			continue
		}
		rel := gitx.Relativize(repo.Root, p)
		if rel == "" {
			rel = gitx.Relativize(resolved(repo.Root), filepath.Join(resolved(dir), filepath.Base(p)))
		}
		if rel == "" {
			continue
		}
		i := slices.IndexFunc(out, func(t touched) bool { return samePath(t.repo.Root, repo.Root) })
		if i < 0 {
			i = len(out)
			out = append(out, touched{repo: repo})
		}
		out[i].files = append(out[i].files, rel)
	}
	for i := range out {
		slices.Sort(out[i].files)
		out[i].files = slices.Compact(out[i].files)
	}
	return out
}

// checkoutOf is the checkout holding dir, judged from the filesystem alone: r when dir is in
// r or no checkout can be read there (an inherited GIT_DIR names r's), another one only if
// the team policy admits it, else nil.
func (e Env) checkoutOf(r *Repo, dir string) *Repo {
	root, gitDir, ok := gitx.LocateFS(dir)
	if !ok || samePath(root, r.Root) || samePath(gitDir, r.GitDir) {
		return r
	}
	origin, remote := gitx.OriginFS(gitDir)
	id := config.Repository{Origin: origin}
	if !e.Policy.Admits(id) {
		return nil
	}
	// Reported only: no store, and never passed to EmitFor, which would claim with it.
	return &Repo{Root: root, GitDir: gitDir, ProjectID: r.ProjectID, Repository: id, Remote: remote}
}

// sameRepository reports whether two checkouts share one git repository: a main checkout and
// its linked worktrees do.
func sameRepository(a, b *Repo) bool {
	return samePath(gitx.CommonDirFS(a.GitDir), gitx.CommonDirFS(b.GitDir))
}

// samePath reports whether a and b name one path once symlinks are resolved: an agent may
// report its cwd resolved (/private/tmp/x on macOS) and a file under the link (/tmp/x/y).
func samePath(a, b string) bool {
	return a == b || resolved(a) == resolved(b)
}

// resolved is p with symlinks resolved, or p itself when it cannot be.
func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}
