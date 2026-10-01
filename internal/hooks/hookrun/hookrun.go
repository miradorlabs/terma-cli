// Package hookrun is the agent-neutral runtime behind `terma hook <event>`: parse the
// payload, update local state, append to the spool, never touch the network. Every
// handler returns nil on what it cannot understand, because a hook that fails a commit
// uninstalls the product.
package hookrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/trailer"
)

// ActiveTTL is how long an announced session claims commits no manifest attributes.
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
	Spool   *spool.Spool
	Version string
	Debug   bool
	// OnClaim, when set, is called after a relay claim so the caller can start the relay.
	OnClaim func()
	// Flush, when set, starts detached delivery of what the spool holds.
	Flush func()
	// Policy is the organization's collection policy; global mode places every session in its DefaultProjectID.
	Policy config.Policy
}

// Time is when the hook runs: Now when the caller set it, the clock otherwise.
func (e Env) Time() time.Time {
	if e.Now.IsZero() {
		return time.Now()
	}
	return e.Now
}

// Logf prints a line to Stderr under --debug only, since a hook's output can reach the model.
func (e Env) Logf(format string, args ...any) {
	if e.Debug && e.Stderr != nil {
		fmt.Fprintf(e.Stderr, "terma hook: "+format+"\n", args...)
	}
}

func (e Env) emit(ev spool.Event) {
	if e.Spool == nil {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = e.Time()
	}
	if err := e.Spool.Append(ev); err != nil {
		e.Logf("spool append failed: %v", err)
	}
}

// Repo is the resolved repository for the current directory.
type Repo struct {
	Root   string
	GitDir string
	Store  *session.Store
	// ProjectID is the binding, a linked worktree's falling back to its main checkout's.
	ProjectID string
	// Name is the checkout's directory name, a linked worktree's main checkout's.
	Name string
	// Worktree is git's name for a linked worktree, "" in a main checkout.
	Worktree string
}

// Repo resolves the repository, or outside Git the bound workspace, and its project.
func (e Env) Repo(ctx context.Context) (*Repo, error) {
	// Filesystem first: a git subprocess is a third of the hook budget on macOS.
	root, gitDir, ok := gitx.LocateFS(e.Cwd)
	if !ok {
		var err error
		if root, gitDir, err = project.Locate(ctx, e.Cwd); err != nil {
			return nil, err
		}
	}
	if gitDir == "" {
		// Outside Git only a bound workspace counts, except in global mode.
		if _, err := project.Load(root); err != nil && (!e.Policy.Global() || !errors.Is(err, project.ErrNotFound)) {
			return nil, err
		}
	}
	stateDir, err := project.StateDir(root, gitDir)
	if err != nil {
		return nil, err
	}
	r := &Repo{Root: root, GitDir: gitDir, Store: session.Open(stateDir)}
	r.Name, r.Worktree = CheckoutNames(root, gitDir)
	if e.Policy.Global() {
		r.ProjectID = e.Policy.DefaultProjectID
	} else if f, _, err := project.Resolve(root, gitDir); err == nil {
		r.ProjectID = f.Project.ID
	}
	return r, nil
}

// EmitFor spools an event stamped with the repository's project binding and, from a
// linked worktree, which one.
func (e Env) EmitFor(r *Repo, ev spool.Event) {
	ev.Global = e.Policy.Global()
	if r != nil {
		ev.Workspace = r.Root
		pol := routing.EffectivePolicy(e.Policy, r.ProjectID)
		if pol.ExcludesPath(r.Root, "") || pol.HasExcludedPath(ev.Attrs, r.Root) {
			return
		}
	}
	if r != nil && (r.ProjectID != "" || r.Worktree != "") {
		if ev.Attrs == nil {
			ev.Attrs = map[string]any{}
		}
		if r.ProjectID != "" {
			ev.Attrs[AttrProjectID] = r.ProjectID
		}
		r.StampWorktree(ev.Attrs)
	}
	e.claimForRelay(r, ev)
	e.emit(ev)
}

// claimForRelay claims the event's session for the repository's project on every hook,
// not only at start, since a start hook may never have fired.
func (e Env) claimForRelay(r *Repo, ev spool.Event) {
	if r == nil || r.ProjectID == "" || ev.SessionID == "" || !claim.Enabled() {
		return
	}
	tool, _ := ev.Attrs[AttrTool].(string)
	c := claim.Claim{ProjectID: r.ProjectID, Tool: tool, Repo: r.Name, Worktree: r.Worktree, PIDs: claimPIDs()}
	claim.Write(ev.SessionID, c, e.Time())
	// A subagent whose telemetry uses its agent_id as session id would otherwise be dropped.
	if agent, _ := ev.Attrs[AttrAgentID].(string); agent != "" && agent != ev.SessionID {
		claim.Write(agent, c, e.Time())
	}
	if e.OnClaim != nil {
		e.OnClaim()
	}
}

// StampWorktree names the linked worktree on an event spooled without EmitFor.
func (r *Repo) StampWorktree(attrs map[string]any) {
	if r.Worktree != "" {
		attrs[AttrWorktree] = r.Worktree
	}
}

// CheckoutNames is the repository name events report and, in a linked worktree, git's name for it.
func CheckoutNames(root, gitDir string) (name, worktree string) {
	name = filepath.Base(root)
	if wt, main, ok := gitx.LinkedWorktreeFS(gitDir); ok {
		worktree = wt
		if main != "" {
			name = filepath.Base(main)
		}
	}
	return name, worktree
}

// PrepareCommitMsg stamps a trailer for every session whose manifest meets the staged
// files, or the fresh active one; local state only, under 50 ms.
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
	r, err := env.Repo(ctx)
	if err != nil || r.GitDir == "" {
		return nil
	}
	manifests, err := r.Store.Manifests()
	if err != nil {
		env.Logf("manifests: %v", err)
	}
	active, fresh := r.Store.Active(env.Time(), ActiveTTL)
	if len(manifests) == 0 && (active == nil || !fresh) {
		return nil // nothing could be attributed; skip the git call entirely
	}
	staged, err := gitx.StagedFiles(ctx, r.Root)
	if err != nil {
		env.Logf("staged files: %v", err)
		return nil
	}
	attributions := session.Attribute(staged, manifests, active, fresh)
	if len(attributions) == 0 {
		return nil
	}
	original, err := os.ReadFile(msgPath)
	if err != nil {
		env.Logf("read message: %v", err)
		return nil
	}
	trailers := make([]trailer.Trailer, 0, len(attributions))
	for _, a := range attributions {
		trailers = append(trailers, trailer.Trailer{SessionID: a.SessionID, Tool: a.Tool})
	}
	stamped, changed := trailer.Stamp(string(original), trailers, gitx.CommentCharFS(r.GitDir))
	if !changed {
		return nil
	}
	if err := os.WriteFile(msgPath, []byte(stamped), 0o644); err != nil {
		env.Logf("write message: %v", err)
		return nil
	}
	ids := make([]string, 0, len(attributions))
	for _, a := range attributions {
		ids = append(ids, a.SessionID)
	}
	env.EmitFor(r, spool.Event{Name: EventCommitStamped, SessionID: ids[0], Repo: r.Name, Attrs: map[string]any{
		"sessions": strings.Join(ids, ","), "session_count": len(ids), "staged_count": len(staged), AttrSource: source,
	}})
	return nil
}

// MaxCommitFileStats bounds per-file detail so one sweeping commit cannot push the 16 MiB
// spool's oldest events out; file_count stays the true total.
const MaxCommitFileStats = 50

// commitFileStat counts are pointers so a binary file is not reported as zero lines changed.
type commitFileStat struct {
	Path    string `json:"path"`
	Added   *int   `json:"added,omitempty"`
	Deleted *int   `json:"deleted,omitempty"`
	Binary  bool   `json:"binary,omitempty"`
	// SessionID is set only when the commit carries more than one session.
	SessionID string `json:"session_id,omitempty"`
}

// PostCommit records the commit against its stamped sessions and retires its files from
// their manifests; an unstamped commit gets a count-only event.
func PostCommit(ctx context.Context, env Env) error {
	r, err := env.Repo(ctx)
	if err != nil || r.GitDir == "" {
		return nil
	}
	head, err := gitx.LastCommit(ctx, r.Root)
	if err != nil || head.SHA == "" {
		return nil
	}
	stamped := trailer.Parse(head.Message, gitx.CommentCharFS(r.GitDir))
	if len(stamped) == 0 {
		emitUnattributedCommit(env, r, head) // human-only commit: no manifest to retire
		return nil
	}
	files := head.Paths()
	ids := make([]string, 0, len(stamped))
	for _, t := range stamped {
		ids = append(ids, t.SessionID)
	}
	// Read before Consume empties the manifests.
	var owners map[string]string
	if len(ids) > 1 {
		owners = fileOwners(env, r.Store, ids)
	}
	for _, id := range ids {
		if err := r.Store.Consume(id, files); err != nil {
			env.Logf("consume manifest: %v", err)
		}
	}
	attrs := commitAttrs(r, head)
	attrs["sessions"] = strings.Join(ids, ",")
	attrs["session_count"] = len(ids)
	attrs[AttrTool] = stamped[0].Tool
	addFileStats(env, attrs, head.Files, owners)
	env.EmitFor(r, spool.Event{Name: EventCommit, SessionID: ids[0], Repo: r.Name, Attrs: attrs})
	return nil
}

// emitUnattributedCommit spools the coverage denominator: a commit's identity and size,
// never its file paths, since terma had no part in it. Merges and squashes are skipped
// as prepare-commit-msg skips them.
func emitUnattributedCommit(env Env, r *Repo, head gitx.Commit) {
	if head.IsMerge() || head.IsSquash() {
		return
	}
	env.EmitFor(r, spool.Event{Name: EventCommitUnattributed, Repo: r.Name, Attrs: commitAttrs(r, head)})
}

// commitAttrs is a commit's identity and size, with no second git subprocess and no file names.
func commitAttrs(r *Repo, head gitx.Commit) map[string]any {
	attrs := map[string]any{
		"sha": head.SHA, AttrFileCount: len(head.Files), "author_email": head.AuthorEmail,
	}
	// No diff means no totals, rather than zeros.
	if len(head.Files) > 0 {
		attrs["lines_added"], attrs["lines_deleted"] = lineTotals(head.Files)
	}
	if remote := gitx.RemoteURLFS(r.GitDir); remote != "" {
		attrs["repo_url"] = remote
	}
	if head.Branch != "" {
		attrs["branch"] = head.Branch
	}
	return attrs
}

func lineTotals(stats []gitx.FileStat) (added, deleted int) {
	for _, f := range stats {
		added += f.Added
		deleted += f.Deleted
	}
	return added, deleted
}

// addFileStats attaches the commit's numstat delta, an upper bound on what an agent
// wrote, as a JSON string because delivery flattens attributes to OTLP scalars.
func addFileStats(env Env, attrs map[string]any, stats []gitx.FileStat, owners map[string]string) {
	if len(stats) == 0 {
		return
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
		env.Logf("encode file stats: %v", err)
		return
	}
	attrs["file_stats"] = string(blob)
	attrs["file_stats_reported"] = len(entries)
	attrs["file_stats_truncated"] = len(entries) < len(stats)
}

// fileOwners maps each path to the stamped session that touched it last.
func fileOwners(env Env, store *session.Store, ids []string) map[string]string {
	manifests, err := store.Manifests()
	if err != nil {
		env.Logf("manifests: %v", err)
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
			// Manifests is oldest first, so a tie keeps the earlier session deterministically.
			if prev, seen := touchedAt[f]; seen && !at.After(prev) {
				continue
			}
			owners[f], touchedAt[f] = m.SessionID, at
		}
	}
	return owners
}
