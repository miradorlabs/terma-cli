package hookrun

import (
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
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
// naming its repository. The session's own checkout r also records them in its manifest, for
// the commit stamping, and claims the session; another checkout is reported only if the team
// policy admits it, and a path in no admitted checkout is dropped.
func (e Env) Touch(r *Repo, sess session.Session, toolName string, paths []string, extra map[string]any) {
	for _, c := range e.checkouts(r, paths) {
		if c.repo == r {
			if err := r.Store.Touch(sess, c.files, e.Time()); err != nil {
				e.Logf("record files: %v", err)
				continue
			}
		}
		attrs := map[string]any{
			semconv.GenAIMainAgentNameKey: sess.Tool, semconv.GenAIToolNameKey: toolName, semconv.TermaFilesPathsKey: c.files,
		}
		maps.Copy(attrs, extra)
		if remote := gitx.RemoteURLFS(c.repo.GitDir); remote != "" {
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

// Expect records in sess's manifest the files a tool call is about to write, without
// reporting them: a commit in the same call is stamped, and the hook after the call
// reports what was written.
func (e Env) Expect(r *Repo, sess session.Session, paths []string) {
	for _, c := range e.checkouts(r, paths) {
		if c.repo == r {
			if err := r.Store.Touch(sess, c.files, e.Time()); err != nil {
				e.Logf("record files: %v", err)
			}
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
			continue
		}
		i := slices.IndexFunc(out, func(t touched) bool { return t.repo.Root == repo.Root })
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
	if !ok || root == r.Root || gitDir == r.GitDir {
		return r
	}
	id := config.Repository{Origin: gitx.RepositoryFS(gitDir)}
	if !e.Policy.Admits(id) {
		return nil
	}
	// Reported only: no store, and never passed to EmitFor, which would claim with it.
	return &Repo{Root: root, GitDir: gitDir, ProjectID: r.ProjectID, Repository: id}
}
