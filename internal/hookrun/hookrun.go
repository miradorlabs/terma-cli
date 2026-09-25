// Package hookrun implements `terma hook <event>`: the runtime behind every shim.
//
// Each handler does the least it can and exits: parse what the harness or git
// handed over, update local state under the repo's git dir, append an event to the
// spool. Nothing here talks to the network. The two guardrails are enforced by
// construction — prepare-commit-msg reads and writes local files only, and every
// handler returns nil on anything it cannot understand, because a hook that fails
// a commit uninstalls the product.
package hookrun

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/trailer"
)

// ActiveTTL bounds the fallback: an announced session older than this no longer
// claims commits that no manifest attributes.
const ActiveTTL = 4 * time.Hour

// ManifestRetention is how long a session's touched-files record is kept.
const ManifestRetention = 14 * 24 * time.Hour

// Env is everything a handler needs from the process.
type Env struct {
	Now    time.Time
	Cwd    string
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Spool may be nil (no config dir yet): events are then dropped, never fatal.
	Spool *spool.Spool
	// Version is the terma version, recorded on events.
	Version string
	// Debug prints what happened to Stderr.
	Debug bool
}

func (e Env) now() time.Time {
	if e.Now.IsZero() {
		return time.Now()
	}
	return e.Now
}

func (e Env) logf(format string, args ...any) {
	if e.Debug && e.Stderr != nil {
		fmt.Fprintf(e.Stderr, "terma hook: "+format+"\n", args...)
	}
}

func (e Env) emit(ev spool.Event) {
	if e.Spool == nil {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = e.now()
	}
	if err := e.Spool.Append(ev); err != nil {
		e.logf("spool append failed: %v", err)
	}
}

// repo is the resolved repository for the current directory.
type repo struct {
	root   string
	gitDir string
	store  *session.Store
	// projectID is the committed binding from .terma/settings.json ("" when the repo has
	// not been installed); the flusher routes events to the project's key by it. A linked
	// worktree without a binding of its own has its main checkout's (project.Resolve).
	projectID string
	// name is the repository events report (their repo): the directory name of the
	// checkout, or in a linked worktree of its main checkout, so every worktree of one
	// repository reports as that repository.
	name string
	// worktree is git's name for a linked worktree ("" in a main checkout), sent as
	// AttrWorktree so the work done in one is still told apart.
	worktree string
}

func (e Env) repo(ctx context.Context) (*repo, error) {
	// Filesystem first: a git subprocess is a third of the hook budget on macOS.
	root, gitDir, ok := gitx.LocateFS(e.Cwd)
	if !ok {
		var err error
		if root, gitDir, err = project.Locate(ctx, e.Cwd); err != nil {
			return nil, err
		}
	}
	if gitDir == "" {
		if _, err := project.Load(root); err != nil {
			return nil, err
		}
	}
	stateDir, err := project.StateDir(root, gitDir)
	if err != nil {
		return nil, err
	}
	r := &repo{root: root, gitDir: gitDir, store: session.Open(stateDir)}
	r.name, r.worktree = checkoutNames(root, gitDir)
	if f, _, err := project.Resolve(root, gitDir); err == nil {
		r.projectID = f.Project.ID
	}
	return r, nil
}

// emitFor spools an event stamped with the repository's project binding and, from a
// linked worktree, which one.
func (e Env) emitFor(r *repo, ev spool.Event) {
	if r != nil && (r.projectID != "" || r.worktree != "") {
		if ev.Attrs == nil {
			ev.Attrs = map[string]any{}
		}
		if r.projectID != "" {
			ev.Attrs[AttrProjectID] = r.projectID
		}
		r.stampWorktree(ev.Attrs)
	}
	e.emit(ev)
}

// stampWorktree names the linked worktree an event came from, for the events that are
// spooled without going through emitFor.
func (r *repo) stampWorktree(attrs map[string]any) {
	if r.worktree != "" {
		attrs[AttrWorktree] = r.worktree
	}
}

// checkoutNames names a checkout for events: the repository (in a linked worktree, its
// main checkout's directory name) and, for a linked worktree, git's name for it.
func checkoutNames(root, gitDir string) (name, worktree string) {
	name = filepath.Base(root)
	if wt, main, ok := gitx.LinkedWorktreeFS(gitDir); ok {
		worktree = wt
		if main != "" {
			name = filepath.Base(main)
		}
	}
	return name, worktree
}

// --- Claude Code adapter -----------------------------------------------------------

// claudeHookInput is the JSON Claude Code writes to a hook's stdin. Only the fields
// terma reads are declared; everything else is ignored.
type claudeHookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Event     string `json:"hook_event_name"`
	Source    string `json:"source"`
	Reason    string `json:"reason"`
	Error     string `json:"error"`
	Model     string `json:"model"`
	PromptID  string `json:"prompt_id"`
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`
	ToolName  string `json:"tool_name"`
	ToolUseID string `json:"tool_use_id"`
	ToolInput struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Edits        []struct {
			FilePath string `json:"file_path"`
		} `json:"edits"`
		// SubagentType is the Agent tool's: which kind of subagent was asked for. The
		// task's description and prompt sit beside it and are not decoded.
		SubagentType string `json:"subagent_type"`
	} `json:"tool_input"`
	// ToolResponse is kept undecoded. Only the Agent tool's is ever opened, and then
	// into claudeAgentResult, which names the fields it wants and no others: for an
	// edit this holds the file's content, which is nothing to do with terma.
	ToolResponse json.RawMessage `json:"tool_response"`
}

func readClaudeInput(r io.Reader) (*claudeHookInput, error) {
	in, err := readHookInput[claudeHookInput](r)
	if err != nil {
		return nil, err
	}
	if in.SessionID == "" {
		return nil, errors.New("hook input has no session_id")
	}
	return in, nil
}

const claudeTool = "claude-code"

// SessionStart records the announced session as active and spools the start.
func SessionStart(ctx context.Context, env Env) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil {
		env.logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) {
		env.logf("ignoring unsafe session id")
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		env.logf("not in a git repository: %v", err)
		return nil
	}
	env.announce(r, env.newSession(r, in.SessionID, claudeTool, in.Model), map[string]any{attrSource: in.Source})
	env.captureClaudeAccount(r, in)
	return nil
}

// SessionEnd clears the active session (manifests stay: the work may still be
// uncommitted) and spools the end.
func SessionEnd(ctx context.Context, env Env) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil {
		env.logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) {
		env.logf("ignoring unsafe session id")
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		return nil
	}
	env.endSession(r, in.SessionID, claudeTool, in.Reason)
	return nil
}

// Stop refreshes account evidence before the dispatcher starts delivery.
func Stop(ctx context.Context, env Env) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil {
		env.logf("%v", err)
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		return nil
	}
	env.captureClaudeAccount(r, in)
	return nil
}

// PostToolUse adds the edited files to the session's manifest.
func PostToolUse(ctx context.Context, env Env) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil {
		env.logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) {
		return nil
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.repo(ctx)
	if err != nil {
		return nil
	}
	if isClaudeAgentTool(in.ToolName) {
		env.claudeSubagentCall(r, in)
		return nil
	}
	paths := append([]string{in.ToolInput.FilePath, in.ToolInput.NotebookPath}, editPaths(in)...)
	// Inside a subagent the payload keeps the parent's session_id and names the agent:
	// the manifest stays the session's, the event says which agent did the editing.
	env.touch(r, session.Session{ID: in.SessionID, Tool: claudeTool, Model: in.Model}, in.ToolName,
		relativeFiles(r, env.Cwd, paths), agentAttrs(map[string]any{}, in.AgentID, in.AgentType))
	return nil
}

func editPaths(in *claudeHookInput) []string {
	var out []string
	for _, e := range in.ToolInput.Edits {
		out = append(out, e.FilePath)
	}
	return out
}

// --- git hooks -------------------------------------------------------------------------

// PrepareCommitMsg stamps the message file named by Args[0] with a trailer for
// every session whose manifest intersects the staged files (falling back to a fresh
// active session). Args[1] is git's message source; merges and squashes are left
// alone. Local state only, no network, budgeted well under 50 ms.
func PrepareCommitMsg(ctx context.Context, env Env) error {
	if len(env.Args) == 0 {
		return nil
	}
	msgPath := env.Args[0]
	source := ""
	if len(env.Args) > 1 {
		source = env.Args[1]
	}
	switch source {
	case "merge", "squash":
		return nil
	}
	r, err := env.repo(ctx)
	if err != nil || r.gitDir == "" {
		return nil
	}
	manifests, err := r.store.Manifests()
	if err != nil {
		env.logf("manifests: %v", err)
	}
	active, fresh := r.store.Active(env.now(), ActiveTTL)
	if len(manifests) == 0 && (active == nil || !fresh) {
		return nil // nothing could be attributed; skip the git call entirely
	}
	staged, err := gitx.StagedFiles(ctx, r.root)
	if err != nil {
		env.logf("staged files: %v", err)
		return nil
	}
	attributions := session.Attribute(staged, manifests, active, fresh)
	if len(attributions) == 0 {
		return nil
	}
	original, err := os.ReadFile(msgPath)
	if err != nil {
		env.logf("read message: %v", err)
		return nil
	}
	trailers := make([]trailer.Trailer, 0, len(attributions))
	for _, a := range attributions {
		trailers = append(trailers, trailer.Trailer{SessionID: a.SessionID, Tool: a.Tool})
	}
	stamped, changed := trailer.Stamp(string(original), trailers, gitx.CommentCharFS(r.gitDir))
	if !changed {
		return nil
	}
	if err := os.WriteFile(msgPath, []byte(stamped), 0o644); err != nil {
		env.logf("write message: %v", err)
		return nil
	}
	ids := make([]string, 0, len(attributions))
	for _, a := range attributions {
		ids = append(ids, a.SessionID)
	}
	env.emitFor(r, spool.Event{Name: EventCommitStamped, SessionID: ids[0], Repo: r.name, Attrs: map[string]any{
		"sessions": strings.Join(ids, ","), "session_count": len(ids), "staged_count": len(staged), attrSource: source,
	}})
	return nil
}

// MaxCommitFileStats bounds the per-file detail on a commit event. A vendored
// dependency bump or a formatting sweep touches thousands of files, and every
// event lands in a JSONL spool with a 16 MB ceiling that drops the oldest events
// when it is hit — one commit must not push everything else out of the queue.
// `file_count` stays the true total and `file_stats_truncated` says the list was
// cut, so the number is never quietly wrong.
const MaxCommitFileStats = 50

// commitFileStat is one entry of the `file_stats` attribute.
//
// Added and Deleted are pointers so a binary file, for which git reports no counts
// at all, is encoded as `{"path":…,"binary":true}` rather than as a zero that reads
// like "nothing changed".
type commitFileStat struct {
	Path    string `json:"path"`
	Added   *int   `json:"added,omitempty"`
	Deleted *int   `json:"deleted,omitempty"`
	Binary  bool   `json:"binary,omitempty"`
	// SessionID is the session whose manifest claims this file, set only when the
	// commit carries more than one session (with one, every file is that session's).
	SessionID string `json:"session_id,omitempty"`
}

// PostCommit records the new commit's sha against the sessions stamped into its
// message and retires the committed files from their manifests, so the next commit
// is not attributed to work that already shipped. A commit with no trailer gets a
// count-only event instead, so the share of commits terma attributed is measurable.
func PostCommit(ctx context.Context, env Env) error {
	r, err := env.repo(ctx)
	if err != nil || r.gitDir == "" {
		return nil
	}
	head, err := gitx.LastCommit(ctx, r.root)
	if err != nil || head.SHA == "" {
		return nil
	}
	stamped := trailer.Parse(head.Message, gitx.CommentCharFS(r.gitDir))
	if len(stamped) == 0 {
		emitUnattributedCommit(env, r, head) // human-only commit: no manifest to retire
		return nil
	}
	files := head.Paths()
	ids := make([]string, 0, len(stamped))
	for _, t := range stamped {
		ids = append(ids, t.SessionID)
	}
	// Which session touched which file, read *before* Consume below empties the
	// manifests. Only a multi-session commit needs it: when one session is stamped,
	// every file entry belongs to it and `sessions` already says which.
	var owners map[string]string
	if len(ids) > 1 {
		owners = fileOwners(env, r.store, ids)
	}
	for _, id := range ids {
		if err := r.store.Consume(id, files); err != nil {
			env.logf("consume manifest: %v", err)
		}
	}
	attrs := commitAttrs(r, head)
	attrs["sessions"] = strings.Join(ids, ",")
	attrs["session_count"] = len(ids)
	attrs[attrTool] = stamped[0].Tool
	addFileStats(env, attrs, head.Files, owners)
	env.emitFor(r, spool.Event{Name: EventCommit, SessionID: ids[0], Repo: r.name, Attrs: attrs})
	return nil
}

// emitUnattributedCommit spools the count-only event for a commit that carries no
// trailer: the denominator of "what share of commits did terma attribute".
//
// Why a separate event name and not a `terma.commit` with an attributed=false
// flag: every consumer of `terma.commit` relies on `sessions` always being present
// (the dashboard is built against that), and a flag would make `sessions`,
// `session_count`, `tool` and `file_stats` conditional on every one of them. With
// its own name, nothing that reads `terma.commit` has to change, neither event
// needs a discriminator attribute, and coverage is still one log filter:
// `event.name IN ('terma.commit', 'terma.commit.unattributed')`, grouped by name.
// (Not a prefix match: `terma.commit.stamped` is prepare-commit-msg's record of the
// same commit and would double count.)
//
// THE BOUNDARY. Terma had no part in this commit, so this event is a count, never
// a manifest of what a human changed. It carries the attributes both commit events
// share to identify and size a commit — sha, repo_url, branch, author_email,
// file_count, lines_added, lines_deleted, plus terma.repo and project_id — and
// nothing about the contents: no file paths, no `file_stats`. Anything added here
// has to pass that test. The line totals are the commit's size, not what was in
// it, and they are the same numbers `terma.commit` reports for a stamped commit, so
// the two populations stay comparable.
//
// Merge and squash commits are skipped the way prepare-commit-msg skips them, or
// they would sit in the denominator of a ratio they can never join. post-commit
// gets no source argument, so a merge is read off the parent count that came with
// the one git log call, and a squash off git's default squash subject — the only
// trace once SQUASH_MSG is unlinked; a squash whose message was rewritten counts as
// the ordinary commit it is.
func emitUnattributedCommit(env Env, r *repo, head gitx.Commit) {
	if head.IsMerge() || head.IsSquash() {
		return
	}
	env.emitFor(r, spool.Event{Name: EventCommitUnattributed, Repo: r.name, Attrs: commitAttrs(r, head)})
}

// commitAttrs is a commit's identity and size — what both commit events share.
// Everything here was already in hand after post-commit's one `git log`, except
// the remote, which is read from the config file the way core.commentChar is, so
// no second subprocess is needed. Nothing in here names a file.
func commitAttrs(r *repo, head gitx.Commit) map[string]any {
	attrs := map[string]any{
		"sha": head.SHA, attrFileCount: len(head.Files), "author_email": head.AuthorEmail,
	}
	// A merge or an empty commit has no diff: no totals, rather than zeros that
	// read like "nothing changed".
	if len(head.Files) > 0 {
		attrs["lines_added"], attrs["lines_deleted"] = lineTotals(head.Files)
	}
	// The repo label is only a directory name, which cannot address a commit
	// anywhere. The remote is what turns a sha into a link, so it rides along —
	// stripped of credentials by NormalizeRemote, since this is telemetry. The
	// branch came free with the log call.
	if remote := gitx.RemoteURLFS(r.gitDir); remote != "" {
		attrs["repo_url"] = remote
	}
	if head.Branch != "" {
		attrs["branch"] = head.Branch
	}
	return attrs
}

// lineTotals sums the commit's delta. A binary file has no counts and adds zero.
func lineTotals(stats []gitx.FileStat) (added, deleted int) {
	for _, f := range stats {
		added += f.Added
		deleted += f.Deleted
	}
	return added, deleted
}

// addFileStats attaches the commit's per-file line detail to the event.
//
// READ THIS BEFORE LABELLING THESE NUMBERS IN A UI. The per-file `added`/`deleted`
// here, like the `lines_added` / `lines_deleted` totals from commitAttrs, are the
// *commit's* delta, straight from `git --numstat`. They are not a measurement of
// what the agent wrote. If a human edited the same file in the same commit, or
// hand-fixed the agent's work before committing, their lines are inside these
// numbers and nothing here can separate them: the manifests record that a session
// *touched* a file, not which lines it produced. `session_id` on an entry says
// which session touched that file, with the same caveat — a file both a session
// and a human edited is still attributed to the session. Treat every one of these
// as an upper bound on agent-written change, and label it as one.
//
// The list is encoded as a JSON string rather than as nested values because the
// attributes are flattened to OTLP scalars on delivery: a nested array would be
// rendered with fmt.Sprint and arrive as unparseable Go syntax.
func addFileStats(env Env, attrs map[string]any, stats []gitx.FileStat, owners map[string]string) {
	if len(stats) == 0 {
		return // a merge or an empty commit has no diff to report
	}
	reported := stats
	if len(reported) > MaxCommitFileStats {
		reported = reported[:MaxCommitFileStats]
	}
	entries := make([]commitFileStat, 0, len(reported))
	for _, f := range reported {
		e := commitFileStat{Path: f.Path, SessionID: owners[session.Normalize(f.Path)]}
		if f.Binary {
			e.Binary = true
		} else {
			added, deleted := f.Added, f.Deleted
			e.Added, e.Deleted = &added, &deleted
		}
		entries = append(entries, e)
	}
	blob, err := json.Marshal(entries)
	if err != nil {
		env.logf("encode file stats: %v", err)
		return
	}
	attrs["file_stats"] = string(blob)
	attrs["file_stats_reported"] = len(entries)
	attrs["file_stats_truncated"] = len(entries) < len(stats)
}

// fileOwners maps each repo-relative path to the stamped session that touched it
// last. Manifests are read whole (they are small, local JSON) and must be read
// before Consume, which is what empties them.
func fileOwners(env Env, store *session.Store, ids []string) map[string]string {
	manifests, err := store.Manifests()
	if err != nil {
		env.logf("manifests: %v", err)
		return nil
	}
	stamped := make(map[string]bool, len(ids))
	for _, id := range ids {
		stamped[id] = true
	}
	owners := make(map[string]string)
	touchedAt := make(map[string]time.Time)
	for _, m := range manifests {
		if !stamped[m.SessionID] {
			continue
		}
		for f, at := range m.Files {
			// Two sessions can both have touched a file. The later touch wins;
			// Manifests is ordered oldest session first, so an exact tie keeps the
			// session that started first and the result is deterministic.
			if prev, seen := touchedAt[f]; seen && !at.After(prev) {
				continue
			}
			owners[f], touchedAt[f] = m.SessionID, at
		}
	}
	return owners
}
