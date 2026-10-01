package hookrun

import (
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// The session start, end and edit steps every agent shares, so the agents cannot drift apart.

// NewSession is the record of a session seen now in r.
func (e Env) NewSession(r *Repo, id, tool, model string) session.Session {
	now := e.Time()
	return session.Session{ID: id, Tool: tool, Model: model, Cwd: r.Root, StartedAt: now, UpdatedAt: now}
}

// SetActive records sess as the session an unattributed commit falls back to; a failure is only logged.
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
	attrs := map[string]any{AttrTool: sess.Tool, AttrModel: sess.Model, AttrVersion: e.Version}
	maps.Copy(attrs, extra)
	e.EmitFor(r, spool.Event{Name: EventSessionStart, SessionID: sess.ID, Repo: r.Name, Attrs: attrs})
}

// Announce makes the session active, ages out old manifests and spools the start.
func (e Env) Announce(r *Repo, sess session.Session, extra map[string]any) {
	e.SetActive(r, sess)
	e.PruneManifests(r, sess.UpdatedAt)
	e.EmitStart(r, sess, extra)
}

// EndSession clears the active session and spools the end; manifests stay, the work may be uncommitted.
func (e Env) EndSession(r *Repo, id, tool, reason string) {
	_ = r.Store.ClearActive(id)
	e.EmitFor(r, spool.Event{Name: EventSessionEnd, SessionID: id, Repo: r.Name, Attrs: map[string]any{
		AttrTool: tool, AttrReason: reason,
	}})
}

// Touch adds files to the session's manifest and spools them, only if the manifest took them.
func (e Env) Touch(r *Repo, sess session.Session, toolName string, files []string, extra map[string]any) {
	if len(files) == 0 {
		return
	}
	if err := r.Store.Touch(sess, files, e.Time()); err != nil {
		e.Logf("record files: %v", err)
		return
	}
	attrs := map[string]any{
		AttrTool: sess.Tool, AttrToolName: toolName, "files": strings.Join(files, ","), AttrFileCount: len(files),
	}
	maps.Copy(attrs, extra)
	e.EmitFor(r, spool.Event{Name: EventFilesTouched, SessionID: sess.ID, Repo: r.Name, Attrs: attrs})
}

// RelativeFiles keeps the reported paths inside the repository, repo-relative and in reported order.
func RelativeFiles(r *Repo, cwd string, paths []string) []string {
	var files []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		if rel := gitx.Relativize(r.Root, p); rel != "" {
			files = append(files, rel)
		}
	}
	return files
}

// UniqueSorted reports a file list as a set, for the agents whose lists are sets.
func UniqueSorted(files []string) []string {
	slices.Sort(files)
	return slices.Compact(files)
}
