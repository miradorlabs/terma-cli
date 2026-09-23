package shim

import (
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"slices"
)

// router carries the per-agent knowledge Exec needs to point one agent at the project a
// repository is bound to. One implementation per routable agent lives in its own file
// (router_claude.go, router_codex.go); neither this file nor shim.go switches on an agent
// name, so adding a routable agent is one new router plus one line in the registry.
type router interface {
	// name is the agent's binary name, and its key in the keystore and the record.
	name() string
	// workingDir resolves the directory whose binding applies. Most agents run in the
	// shell's cwd, but some take an explicit working-directory flag that moves it.
	workingDir(cwd string, userArgs []string) string
	// routeArgs returns the arguments to place ahead of the agent's own so it reports to
	// the record's project, or nil to pass through untouched. nil is the answer whenever
	// routing must not apply — no key yet, the user already configured it, or preparation
	// failed — because a half-configured export is worse than none.
	routeArgs(rec Record, userArgs []string) []string
}

// routers is the registry of routable agents, in display order.
var routers = []router{claudeRouter{}, codexRouter{}}

func routerFor(agent string) (router, bool) {
	for _, r := range routers {
		if r.name() == agent {
			return r, true
		}
	}
	return nil, false
}

// Routable reports whether an agent can be routed per-repo.
func Routable(agent string) bool {
	_, ok := routerFor(agent)
	return ok
}

// routeFor resolves the route for an agent invoked in cwd with userArgs. It returns the
// zero route (pass through untouched) whenever routing does not apply: the agent is not
// routable, cwd is not in a bound repo, there is no record, the agent is not routed for
// the project, or its per-project setup is missing.
func routeFor(agent, cwd string, userArgs []string) route {
	r, ok := routerFor(agent)
	if !ok {
		return route{}
	}
	root, err := termaproject.Find(r.workingDir(cwd, userArgs))
	if err != nil {
		return route{}
	}
	f, err := termaproject.Load(root)
	if err != nil {
		return route{}
	}
	rec, ok, err := LoadRecord(f.Project.ID)
	if err != nil || !ok || !slices.Contains(rec.Harnesses, agent) {
		return route{}
	}
	if args := r.routeArgs(rec, userArgs); len(args) > 0 {
		return route{args: args}
	}
	return route{}
}
