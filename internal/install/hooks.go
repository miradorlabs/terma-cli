package install

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// HookPlan is everything install would write into the repository for hooks.
type HookPlan struct {
	reg    *agents.Registry
	det    hookmgr.Detection
	hooks  hookmgr.Plan
	agents []hookmgr.Plan
}

// PlanHooks plans the commit hooks through det's manager and the adapters' own hooks.
func PlanHooks(reg *agents.Registry, root string, det hookmgr.Detection, adapters []string) (HookPlan, error) {
	var hooks hookmgr.Plan
	var err error
	if det.Manager != "" {
		hooks, err = hookmgr.PlanInstall(root, det)
		if err != nil {
			return HookPlan{}, err
		}
	}
	agents, err := PlanAdapters(reg, root, adapters, true)
	if err != nil {
		return HookPlan{}, err
	}
	if err := hookmgr.Validate(root, hooks); err != nil {
		return HookPlan{}, err
	}
	return HookPlan{reg: reg, det: det, hooks: hooks, agents: agents}, nil
}

// Empty reports whether the commit hooks and every agent's hooks are already in place.
func (p HookPlan) Empty() bool {
	if !p.hooks.Empty() {
		return false
	}
	for _, a := range p.agents {
		if !a.Empty() {
			return false
		}
	}
	return true
}

// Print lists the files the hook install would write, or says there are none.
func (p HookPlan) Print(out io.Writer) {
	if p.Empty() {
		fmt.Fprintln(out, "\nHooks already present — nothing to write.")
		return
	}
	if p.det.Manager == "" {
		fmt.Fprintln(out, "\nAgent hooks:")
	} else {
		fmt.Fprintf(out, "\nHooks — commit stamping via %s (%s):\n", p.det.Manager, p.det.Detail)
	}
	for _, c := range p.hooks.Changes {
		fmt.Fprintf(out, "  %-7s %s\n", c.Action(), c.Path)
	}
	for _, a := range p.agents {
		for _, c := range a.Changes {
			fmt.Fprintf(out, "  %-7s %s\n", c.Action(), c.Path)
		}
	}
	// Without the manager's per-clone step the committed hooks never run, silently.
	if len(p.hooks.Notes) > 0 {
		fmt.Fprintln(out, "\nAfter merging:")
		for _, n := range p.hooks.Notes {
			fmt.Fprintf(out, "  - %s\n", n)
		}
	}
}

// Files names what the plan writes, terma's hook shims as their directory.
func (p HookPlan) Files() []string {
	var files []string
	for _, path := range p.Paths() {
		if path = shownPath(path); !slices.Contains(files, path) {
			files = append(files, path)
		}
	}
	return files
}

func shownPath(path string) string {
	if strings.HasPrefix(path, hookmgr.ShimDir+"/") {
		return hookmgr.ShimDir + "/"
	}
	return path
}

// Explain says what each file in Files is for, a line apiece, and what committing them means.
func (p HookPlan) Explain() []string {
	what := map[string]string{}
	for _, c := range p.hooks.Changes {
		what[shownPath(c.Path)] = "stamps each commit with the agent session that wrote it"
	}
	for _, a := range p.reg.All() {
		if path := a.HooksPath(); path != "" {
			what[path] = "reports each " + a.DisplayName() + " session and the files it edits"
		}
	}
	files := p.Files()
	width := 0
	for _, f := range files {
		width = max(width, len(f))
	}
	lines := make([]string, 0, len(files)+2)
	for _, f := range files {
		desc, ok := what[f]
		if !ok {
			desc = "terma's hook wiring"
		}
		lines = append(lines, fmt.Sprintf("%-*s  %s", width, f, desc))
	}
	return append(lines,
		"These are committed: merging them sets up everyone who clones the repository,",
		"and on a machine without terma they do nothing.")
}

// Summary says what the hooks do once installed.
func (p HookPlan) Summary(adapters []string) string {
	var parts []string
	if p.det.Manager != "" {
		parts = append(parts, "commit stamping via "+string(p.det.Manager))
	}
	var agents []string
	for _, name := range adapters {
		if a, ok := p.reg.Lookup(name); ok {
			agents = append(agents, a.DisplayName())
		}
	}
	if len(agents) > 0 {
		parts = append(parts, "session hooks for "+output.And(agents))
	}
	if len(parts) == 0 {
		return "none to write"
	}
	return strings.Join(parts, "; ")
}

// Paths lists the files the plan writes or deletes, relative to the root, in Print's order.
func (p HookPlan) Paths() []string {
	var paths []string
	for _, c := range p.hooks.Changes {
		paths = append(paths, c.Path)
	}
	for _, a := range p.agents {
		for _, c := range a.Changes {
			paths = append(paths, c.Path)
		}
	}
	return paths
}

// Apply writes the plan into the repository at root.
func (p HookPlan) Apply(root string) error {
	if err := hookmgr.Apply(root, p.hooks); err != nil {
		return err
	}
	for _, a := range p.agents {
		if err := hookmgr.Apply(root, a); err != nil {
			return err
		}
	}
	return nil
}

func managerOrEmpty(det hookmgr.Detection, installed bool) string {
	if !installed {
		return ""
	}
	return string(det.Manager)
}

func hooksOrNil(installed bool) []string {
	if !installed {
		return nil
	}
	return hookmgr.GitHooks
}

// PlanAdapters computes each named adapter's repository changes, in registry order.
func PlanAdapters(reg *agents.Registry, root string, names []string, install bool) ([]hookmgr.Plan, error) {
	want := map[string]bool{}
	for _, name := range names {
		if _, ok := reg.Lookup(name); !ok {
			return nil, fmt.Errorf("unknown agent %q (want %s)", name, output.And(reg.RepoNames()))
		}
		want[name] = true
	}
	var plans []hookmgr.Plan
	for _, a := range reg.All() {
		if !want[a.Name()] {
			continue
		}
		p, err := a.Plan(root, install)
		if err != nil {
			return nil, err
		}
		if err := hookmgr.Validate(root, p); err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	return plans, nil
}

// ExistingOnly keeps the changes to files already there, what a refresh rewrites.
func (p HookPlan) ExistingOnly() HookPlan {
	p.hooks = existingOnly(p.hooks)
	p.agents = slices.Clone(p.agents)
	for i := range p.agents {
		p.agents[i] = existingOnly(p.agents[i])
	}
	return p
}

func existingOnly(p hookmgr.Plan) hookmgr.Plan {
	p.Changes = slices.DeleteFunc(slices.Clone(p.Changes), func(c hookmgr.Change) bool { return c.Before == nil })
	p.Notes = nil
	return p
}
