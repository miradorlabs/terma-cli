// Package hookrun is the agent-neutral runtime behind `terma hook <event>`: parse the
// payload, update local state, append to the spool, never touch the network. Every
// handler returns nil on what it cannot understand, because a hook that fails a commit
// uninstalls the product.
package hookrun

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

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
	// ConfigDir is terma's config directory, which holds what setup recorded.
	ConfigDir string
	// StateDir is terma's state directory, where hooks keep their state.
	StateDir string
	// Spool may be nil (it could not be opened): events are then dropped, never fatal.
	Spool   *spool.Spool
	Version string
	Debug   bool
	// OnClaim, when set, is called after a relay claim with the claimed checkout's root, so
	// the caller can start the relay and wire that clone.
	OnClaim func(root string)
	// Flush, when set, starts detached delivery of what the spool holds, refreshing a stale
	// policy first.
	Flush func()
	// AwaitPush, when set, starts a detached AwaitPush for the push recorded at path.
	AwaitPush func(path string)
	// Policy is the organization's collection policy; global mode places every session in its DefaultProjectID.
	Policy config.Policy
	// Team is the developer's team from setup, which claims every session they run in a
	// collected repository, whatever the policy's state.
	Team string
	// Agents are the agents the developer chose at setup.
	Agents []string
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

func (e Env) emit(ev spool.Event) bool {
	if e.Spool == nil {
		return false
	}
	if ev.Time.IsZero() {
		ev.Time = e.Time()
	}
	if err := e.Spool.Append(ev); err != nil {
		e.Logf("spool append failed: %v", err)
		return false
	}
	return true
}

// Repo is the resolved repository for the current directory.
type Repo struct {
	Root   string
	GitDir string
	Store  *session.Store
	// ProjectID is the developer's team, or global mode's default project.
	ProjectID string
	// Name is the checkout's directory name, a linked worktree's main checkout's.
	Name string
	// Worktree is git's name for a linked worktree, "" in a main checkout.
	Worktree string
	// Repository is the working copy as the team policy's repository list names it.
	Repository config.Repository
}

// workTree is the checkout's root inside git, "" for a folder outside it: only a working
// tree is a repository root to the platform. It is symlink-resolved, as the platform compares
// it, however the hook's cwd reached the checkout (/tmp is /private/tmp on macOS).
func (r *Repo) workTree() string {
	if r == nil || r.GitDir == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(r.Root); err == nil {
		return resolved
	}
	return r.Root
}

// ErrNotAdmitted is a working copy the team policy does not collect: its hooks record nothing.
var ErrNotAdmitted = errors.New("not among the team's repositories")

// Open is every session hook's first step: it refuses an unsafe session id, runs from
// the payload's cwd when it names one, and resolves the repository, logging why when it
// cannot.
func (e *Env) Open(ctx context.Context, sessionID, cwd string) (*Repo, bool) {
	if !session.ValidID(sessionID) {
		e.Logf("ignoring unsafe session id")
		return nil, false
	}
	e.Cwd = cmp.Or(cwd, e.Cwd)
	r, err := e.Repo(ctx)
	if err != nil {
		e.Logf("no repository to record: %v", err)
		return nil, false
	}
	return r, true
}

// Repo resolves the repository, or outside Git the current directory, and its project,
// or ErrNotAdmitted, before anything is written, for one the team policy does not collect.
// It first hands on what hooks in an agent's sandbox parked there (unpark).
func (e Env) Repo(ctx context.Context) (*Repo, error) {
	r, err := e.resolve(ctx)
	if err == nil {
		e.unpark(r)
	}
	return r, err
}

// resolve is Repo without the unpark, for prepare-commit-msg, which has a commit's latency
// budget to keep: the next hook hands the parked records on.
func (e Env) resolve(ctx context.Context) (*Repo, error) {
	root, gitDir, id, err := e.locate(ctx)
	if err != nil {
		return nil, err
	}
	if !e.Policy.Admits(id) {
		return nil, ErrNotAdmitted
	}
	storeDir, err := project.StoreDir(e.StateDir, root, gitDir)
	if err != nil {
		return nil, err
	}
	r := &Repo{Root: root, GitDir: gitDir, Store: session.Open(storeDir), Repository: id}
	r.Name, r.Worktree = CheckoutNames(root, gitDir)
	r.ProjectID = e.Team
	if e.Policy.Global() {
		r.ProjectID = e.Policy.DefaultProjectID
	}
	return r, nil
}

// parking is where a git hook in an agent's sandbox parks what the state directory refused:
// the repository's git directory, which the sandbox lets it write, even where the session
// store is a workspace's under the state directory.
func (r *Repo) parking() *session.Store {
	return session.Open(filepath.Join(r.GitDir, project.GitStoreDir))
}

// What a git hook run in an agent's sandbox could not write to the state directory waits in
// the repository's git directory, which it can write, under these names.
const (
	parkedEvents = "events"
	parkedPushes = "pushes"
)

// park keeps v, which the state directory refused, in the repository's git directory under name;
// the next hook there that reaches the state directory unparks it.
func (e Env) park(r *Repo, name string, v any) bool {
	if r == nil || r.GitDir == "" {
		return false // a folder outside git keeps its store in the state directory too
	}
	line, err := json.Marshal(v)
	if err == nil {
		err = r.parking().Park(name, append(line, '\n'))
	}
	if err != nil {
		e.Logf("park %s: %v", name, err)
		return false
	}
	return true
}

// unpark hands on what sandboxed hooks parked in r's git directory: events to the spool, in
// one append, then a flush, and pushes to AwaitPush.
func (e Env) unpark(r *Repo) {
	if r.GitDir == "" {
		return
	}
	e.unparkPushes(r)
	if e.Spool == nil {
		return
	}
	moved := false
	err := r.parking().Unpark(parkedEvents, func(lines []byte) error {
		var events []spool.Event
		for line := range bytes.Lines(lines) {
			var ev spool.Event
			if json.Unmarshal(line, &ev) == nil {
				events = append(events, ev)
			}
		}
		moved = len(events) > 0
		return e.Spool.Append(events...)
	})
	if err != nil {
		e.Logf("unpark events: %v", err)
		return
	}
	if moved && e.Flush != nil {
		e.Flush()
	}
}

// Admits reports whether the team policy collects the working copy at dir, judged as Repo
// judges the current directory.
func (e Env) Admits(ctx context.Context, dir string) bool {
	e.Cwd = dir
	_, _, id, err := e.locate(ctx)
	return err == nil && e.Policy.Admits(id)
}

// locate finds the working copy at Cwd, a Git checkout's root from any directory in it,
// and how admission names it.
func (e Env) locate(ctx context.Context) (root, gitDir string, id config.Repository, err error) {
	// Filesystem first: a git subprocess is a third of the hook budget on macOS.
	root, gitDir, ok := gitx.LocateFS(e.Cwd)
	if !ok {
		if root, gitDir, err = project.Locate(ctx, e.Cwd); err != nil {
			return "", "", id, err
		}
	}
	id.Origin = gitx.RepositoryFS(gitDir)
	return root, gitDir, id, nil
}

// EmitFor spools an event stamped with the repository's project, parking it in the
// repository's store when the spool cannot be written, and reports whether it was kept.
func (e Env) EmitFor(r *Repo, ev spool.Event) bool {
	ev = e.stamp(r, ev)
	e.claimForRelay(r, ev)
	if ev.Time.IsZero() {
		ev.Time = e.Time()
	}
	return e.emit(ev) || e.park(r, parkedEvents, ev)
}

// stamp names the event's repository, for delivery's admission, and its project.
func (e Env) stamp(r *Repo, ev spool.Event) spool.Event {
	ev.Global = e.Policy.Global()
	if r != nil {
		ev.Repository = r.Repository
	}
	if r != nil && r.ProjectID != "" {
		if ev.Attrs == nil {
			ev.Attrs = map[string]any{}
		}
		ev.Attrs[AttrProjectID] = r.ProjectID
	}
	return ev
}

// claimForRelay claims the event's session for the repository's project on every hook,
// not only at start, since a start hook may never have fired.
func (e Env) claimForRelay(r *Repo, ev spool.Event) {
	if r == nil || r.ProjectID == "" || ev.SessionID == "" {
		return
	}
	tool, _ := ev.Attrs[semconv.GenAIMainAgentNameKey].(string)
	c := claim.Claim{ProjectID: r.ProjectID, Tool: tool, Repo: r.Name, Worktree: r.Worktree, Repository: r.Repository, Root: r.workTree(), Cwd: e.workingDir(), PIDs: claimPIDs()}
	claim.Write(e.StateDir, ev.SessionID, c, e.Time())
	// A subagent whose telemetry uses its agent id as session id would otherwise be dropped.
	if agent, _ := ev.Attrs[semconv.TermaAgentIDKey].(string); agent != "" && agent != ev.SessionID {
		claim.Write(e.StateDir, agent, c, e.Time())
	}
	if e.OnClaim != nil {
		e.OnClaim(r.Root)
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
