package hookrun

import (
	"cmp"
	"context"
	"errors"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// Admission: which team's policy collects the working copy a hook runs in, and so which
// project its session reports to. One machine collects for several organizations; the
// repository decides.

// policies are the policies this hook admits by.
func (e Env) policies() config.Policies {
	if len(e.Policies) > 0 {
		return e.Policies
	}
	return config.Policies{e.Policy}
}

// admit is the policy that collects the working copy id, if any.
func (e Env) admit(id config.Repository) (config.Policy, bool) {
	return e.policies().Admitting(id)
}

// global is the policy that collects every session on this machine, if one does.
func (e Env) global() (config.Policy, bool) {
	return e.policies().Global()
}

// Repo is the resolved repository for the current directory.
type Repo struct {
	Root   string
	GitDir string
	Store  *session.Store
	// ProjectID is the team that collects this working copy, or global mode's default project.
	ProjectID string
	// Policy is the collection policy that admits this working copy, ProjectID's team's.
	Policy config.Policy
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
// or ErrNotAdmitted, before anything is written, for one no team's policy collects.
func (e Env) Repo(ctx context.Context) (*Repo, error) {
	root, gitDir, id, err := e.locate(ctx)
	if err != nil {
		return nil, err
	}
	pol, ok := e.admit(id)
	if !ok {
		return nil, ErrNotAdmitted
	}
	storeDir, err := project.StoreDir(e.StateDir, root, gitDir)
	if err != nil {
		return nil, err
	}
	r := &Repo{Root: root, GitDir: gitDir, Store: session.Open(storeDir), Repository: id, Policy: pol}
	r.Name, r.Worktree = CheckoutNames(root, gitDir)
	// The admitting policy's team; an offline stub names none, and the developer's team stands in.
	r.ProjectID = cmp.Or(pol.TeamID, e.Team)
	if pol.Global() {
		r.ProjectID = pol.DefaultProjectID
	}
	return r, nil
}

// AdmitsUnder reports whether r's own team collects the working copy at dir, judged as
// Repo judges the current directory: what a session in r reports from elsewhere (a
// thread's earlier turns in another checkout) is stamped with r's team, so the collection
// must give that checkout to the same team, never to another's listing, which may be
// another organization's; a global policy of r's takes only what no listing names.
func (e Env) AdmitsUnder(ctx context.Context, r *Repo, dir string) bool {
	e.Cwd = dir
	_, _, id, err := e.locate(ctx)
	if err != nil {
		return false
	}
	if r == nil {
		_, ok := e.admit(id)
		return ok
	}
	return e.ownAdmits(r, id)
}

// ownAdmits reports whether the working copy id is r's own team's to collect: the
// collection resolves it to that team (config.Policies.Admitting), not to another team's
// listing, which a global policy of r's would otherwise sweep up across organizations.
func (e Env) ownAdmits(r *Repo, id config.Repository) bool {
	pol, ok := e.admit(id)
	return ok && pol.Team() == r.Policy.Team()
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
