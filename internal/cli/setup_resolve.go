package cli

import (
	"fmt"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/ui/prompt"
)

type conflictOption struct {
	label      string
	detail     string
	bindings   []bindingChoice
	resolvedAs string
	leaveOpen  bool
}

// resolveSetupConflicts interactively resolves pending cross-organization policy conflicts
// right inside `terma setup` so developers do not have to run a separate command afterward.
// Non-interactive runs (`-y`, `--desktop-grant`, piped stdin) leave conflicts pending so
// `terma teams --bind/--refuse` and the desktop app UI can resolve them.
func (app *App) resolveSetupConflicts(f setupFlags, conflicts []policyConflict) (extra []bindingChoice, remaining []policyConflict, resolved []string, err error) {
	if f.assumeYes || f.desktopGrant || !app.canAsk() {
		return nil, conflicts, nil, nil
	}
	total := 0
	for _, c := range conflicts {
		if c.severity == conflictPending && len(c.options) > 0 {
			total++
		}
	}
	if total == 0 {
		return nil, conflicts, nil, nil
	}

	idx := 0
	for _, c := range conflicts {
		if c.severity != conflictPending || len(c.options) == 0 {
			remaining = append(remaining, c)
			continue
		}
		idx++
		title := fmt.Sprintf("Policy conflict — %s", c.subject)
		if total > 1 {
			title = fmt.Sprintf("Policy conflict (%d/%d) — %s", idx, total, c.subject)
		}
		items := make([]prompt.Item, len(c.options))
		for i, opt := range c.options {
			items[i] = prompt.Item{
				Label:    opt.label,
				Detail:   opt.detail,
				Selected: i == 0,
			}
		}
		chosen, err := app.runForm(&prompt.Form{Title: title, Items: items, Choice: true})
		if err != nil {
			return nil, nil, nil, err
		}
		picked := -1
		for i, it := range chosen {
			if it.Selected {
				picked = i
				break
			}
		}
		if picked < 0 || picked >= len(c.options) {
			return nil, nil, nil, prompt.ErrCancelled
		}
		opt := c.options[picked]
		if opt.leaveOpen {
			remaining = append(remaining, c)
			continue
		}
		extra = append(extra, opt.bindings...)
		if opt.resolvedAs != "" {
			resolved = append(resolved, opt.resolvedAs)
		}
	}
	return extra, remaining, resolved, nil
}

func exactConflictOptions(repoKey string, orgIDs []string, orgLabel func(string) string) []conflictOption {
	opts := make([]conflictOption, 0, len(orgIDs)+2)
	for _, id := range orgIDs {
		name := orgLabel(id)
		opts = append(opts, conflictOption{
			label:      fmt.Sprintf("Bind to %s", name),
			detail:     fmt.Sprintf("collect %s for %s", repoKey, name),
			bindings:   []bindingChoice{{key: repoKey, org: id}},
			resolvedAs: fmt.Sprintf("%s → %s", repoKey, name),
		})
	}
	opts = append(opts,
		conflictOption{
			label:      "Don't collect",
			detail:     fmt.Sprintf("refuse %s", repoKey),
			bindings:   []bindingChoice{{key: repoKey, refuse: true}},
			resolvedAs: fmt.Sprintf("%s refused", repoKey),
		},
		conflictOption{
			label:     "Decide later",
			detail:    "leave pending (resolve with terma teams)",
			leaveOpen: true,
		},
	)
	return opts
}

func wildcardConflictOptions(owner string, orgIDs []string, orgLabel func(string) string) []conflictOption {
	opts := make([]conflictOption, 0, len(orgIDs)+2)
	for _, id := range orgIDs {
		name := orgLabel(id)
		opts = append(opts, conflictOption{
			label:      fmt.Sprintf("Bind %s/* to %s", owner, name),
			detail:     fmt.Sprintf("all repos under %s", owner),
			bindings:   []bindingChoice{{key: owner, org: id}},
			resolvedAs: fmt.Sprintf("%s/* → %s", owner, name),
		})
	}
	opts = append(opts,
		conflictOption{
			label:      "Don't collect",
			detail:     fmt.Sprintf("refuse %s/*", owner),
			bindings:   []bindingChoice{{key: owner, refuse: true}},
			resolvedAs: fmt.Sprintf("%s/* refused", owner),
		},
		conflictOption{
			label:     "Decide later",
			detail:    "leave pending (resolve with terma teams)",
			leaveOpen: true,
		},
	)
	return opts
}

func wildcardVsExactOptions(owner, wildOrg string, otherRepos []string, exactOrgs map[string][]string, orgLabel func(string) string) []conflictOption {
	wildName := orgLabel(wildOrg)
	var opts []conflictOption

	// When every conflicting exact repository belongs to a single non-wildcard organization,
	// offer the split resolution first so both teams' policies are honored cleanly.
	allSingle := len(otherRepos) > 0
	type repoOwner struct{ repo, org string }
	var splits []repoOwner
	for _, repo := range otherRepos {
		var others []string
		for _, o := range exactOrgs[repo] {
			if o != wildOrg {
				others = appendUnique(others, o)
			}
		}
		if len(others) != 1 {
			allSingle = false
			break
		}
		splits = append(splits, repoOwner{repo: repo, org: others[0]})
	}

	if allSingle {
		splitBindings := make([]bindingChoice, 0, len(splits)+1)
		splitBindings = append(splitBindings, bindingChoice{key: owner, org: wildOrg})
		exactOnlyBindings := make([]bindingChoice, 0, len(splits)+1)
		exactOnlyBindings = append(exactOnlyBindings, bindingChoice{key: owner, refuse: true})
		var exDesc []string
		for _, sp := range splits {
			splitBindings = append(splitBindings, bindingChoice{key: sp.repo, org: sp.org, override: true})
			exactOnlyBindings = append(exactOnlyBindings, bindingChoice{key: sp.repo, org: sp.org, override: true})
			exDesc = append(exDesc, fmt.Sprintf("%s → %s", sp.repo, orgLabel(sp.org)))
		}
		exSummary := strings.Join(exDesc, ", ")
		opts = append(opts,
			conflictOption{
				label:      fmt.Sprintf("%s for %s/* (with exceptions)", wildName, owner),
				detail:     exSummary,
				bindings:   splitBindings,
				resolvedAs: fmt.Sprintf("%s/* → %s (except %s)", owner, wildName, exSummary),
			},
			conflictOption{
				label:      fmt.Sprintf("%s for all of %s/*", wildName, owner),
				detail:     fmt.Sprintf("all repos under %s", owner),
				bindings:   []bindingChoice{{key: owner, org: wildOrg}},
				resolvedAs: fmt.Sprintf("%s/* → %s", owner, wildName),
			},
			conflictOption{
				label:      fmt.Sprintf("Only %s", exSummary),
				detail:     fmt.Sprintf("skip %s/* wildcard", owner),
				bindings:   exactOnlyBindings,
				resolvedAs: fmt.Sprintf("%s (refused %s/*)", exSummary, owner),
			},
		)
	} else {
		opts = append(opts, conflictOption{
			label:      fmt.Sprintf("%s for all of %s/*", wildName, owner),
			detail:     fmt.Sprintf("all repos under %s", owner),
			bindings:   []bindingChoice{{key: owner, org: wildOrg}},
			resolvedAs: fmt.Sprintf("%s/* → %s", owner, wildName),
		})
	}

	opts = append(opts,
		conflictOption{
			label:      "Don't collect",
			detail:     fmt.Sprintf("refuse %s/*", owner),
			bindings:   []bindingChoice{{key: owner, refuse: true}},
			resolvedAs: fmt.Sprintf("%s/* refused", owner),
		},
		conflictOption{
			label:     "Decide later",
			detail:    "leave pending (resolve with terma teams)",
			leaveOpen: true,
		},
	)
	return opts
}

func wildcardVsExistingOptions(owner, wildOrg string, children []string, orgLabel func(string) string) []conflictOption {
	wildName := orgLabel(wildOrg)
	childList := strings.Join(children, ", ")
	return []conflictOption{
		{
			label:      fmt.Sprintf("%s for rest of %s/*", wildName, owner),
			detail:     fmt.Sprintf("keep %s", childList),
			bindings:   []bindingChoice{{key: owner, org: wildOrg}},
			resolvedAs: fmt.Sprintf("%s/* → %s (kept %s)", owner, wildName, childList),
		},
		{
			label:      fmt.Sprintf("Refuse %s/*", owner),
			detail:     fmt.Sprintf("keep %s only", childList),
			bindings:   []bindingChoice{{key: owner, refuse: true}},
			resolvedAs: fmt.Sprintf("%s/* refused (kept %s)", owner, childList),
		},
		{
			label:     "Decide later",
			detail:    "leave pending (resolve with terma teams)",
			leaveOpen: true,
		},
	}
}
