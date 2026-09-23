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

// An adapter reads its agent's payload and decides what it means. What a session's
// start, its end and its edits then do to local state and to the spool is the same for
// every agent, and lives here so the adapters cannot drift apart. The differences that
// are real travel as arguments: the attributes only one agent has, and whether a file
// list is reported as a set.

// newSession is the record of a session seen now in r.
func (e Env) newSession(r *repo, id, tool, model string) session.Session {
	now := e.now()
	return session.Session{ID: id, Tool: tool, Model: model, Cwd: r.root, StartedAt: now, UpdatedAt: now}
}

// setActive records sess as the session an unattributed commit falls back to. A failure
// is logged and the hook carries on: the event is still worth spooling.
func (e Env) setActive(r *repo, sess session.Session) {
	if err := r.store.SetActive(sess); err != nil {
		e.logf("record session: %v", err)
	}
}

// pruneManifests drops touched-files records older than ManifestRetention.
func (e Env) pruneManifests(r *repo, now time.Time) {
	if _, err := r.store.Prune(now.Add(-ManifestRetention)); err != nil {
		e.logf("prune: %v", err)
	}
}

// emitStart spools the session's start. extra is what only one agent knows: where the
// session came from, the session that spawned it.
func (e Env) emitStart(r *repo, sess session.Session, extra map[string]any) {
	attrs := map[string]any{attrTool: sess.Tool, attrModel: sess.Model, attrVersion: e.Version}
	maps.Copy(attrs, extra)
	e.emitFor(r, spool.Event{Name: EventSessionStart, SessionID: sess.ID, Repo: repoName(r.root), Attrs: attrs})
}

// announce is a session's start: the session becomes the active one, old manifests are
// aged out, and the start is spooled. An adapter with work of its own between those
// steps (Codex reads the rollout, Antigravity opens a turn) or without one of them
// (Codex's notify never prunes) composes them itself, in the order it always had.
func (e Env) announce(r *repo, sess session.Session, extra map[string]any) {
	e.setActive(r, sess)
	e.pruneManifests(r, sess.UpdatedAt)
	e.emitStart(r, sess, extra)
}

// endSession clears the active session and spools the end. Manifests stay: the work may
// still be uncommitted.
func (e Env) endSession(r *repo, id, tool, reason string) {
	_ = r.store.ClearActive(id)
	e.emitFor(r, spool.Event{Name: EventSessionEnd, SessionID: id, Repo: repoName(r.root), Attrs: map[string]any{
		attrTool: tool, attrReason: reason,
	}})
}

// touch adds repo-relative files to the session's manifest and spools them. toolName is
// the agent's name for what made the edit. Nothing is spooled when the manifest did not
// take the files: the event would claim an attribution no commit can carry.
func (e Env) touch(r *repo, sess session.Session, toolName string, files []string, extra map[string]any) {
	if len(files) == 0 {
		return
	}
	if err := r.store.Touch(sess, files, e.now()); err != nil {
		e.logf("record files: %v", err)
		return
	}
	attrs := map[string]any{
		attrTool: sess.Tool, attrToolName: toolName, "files": strings.Join(files, ","), attrFileCount: len(files),
	}
	maps.Copy(attrs, extra)
	e.emitFor(r, spool.Event{Name: EventFilesTouched, SessionID: sess.ID, Repo: repoName(r.root), Attrs: attrs})
}

// relativeFiles resolves reported paths against the working directory and keeps the
// ones inside the repository, in the order they were reported.
func relativeFiles(r *repo, cwd string, paths []string) []string {
	var files []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		if rel := gitx.Relativize(r.root, p); rel != "" {
			files = append(files, rel)
		}
	}
	return files
}

// uniqueSorted reports a file list as a set. Codex's patches and Cursor's subagent
// reports are spooled this way and the other agents' lists are not, so it is the
// caller's choice rather than relativeFiles' habit.
func uniqueSorted(files []string) []string {
	slices.Sort(files)
	return slices.Compact(files)
}
