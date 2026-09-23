package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/api"
	"github.com/miradorlabs/terma-cli/internal/output"
)

func newPrincipalCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "principal",
		Aliases: []string{"principals"},
		Short:   "Resolve who is behind a session's user or API-key id",
		Hidden:  true,
		Long: `Sessions and usage carry principal ids, never names. The principal catalog maps
each id to the name the provider reported (a user's email, an API key's label) and
to any alias set in the web app.

Every --user and --api-key flag on ` + "`terma session`" + ` and ` + "`terma usage`" + ` accepts a
name as well as an id and resolves it through this catalog, so you rarely need these
commands directly. They are here for when a match is ambiguous, or to see what the
catalog knows.`,
	}
	cmd.AddCommand(newPrincipalListCommand(), newPrincipalFindCommand())
	return cmd
}

func newPrincipalListCommand() *cobra.Command {
	var kind, source string
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List every user and API key the project has seen",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validatePrincipalKind(kind); err != nil {
				return err
			}
			ctx, client, format, err := setupProjectCommand(cmd)
			if err != nil {
				return err
			}
			principals, err := client.AllAIPrincipals(ctx, principalFilter(kind, source))
			if err != nil {
				return err
			}
			sortPrincipals(principals)
			return output.Render(cmd.OutOrStdout(), format, principalTable(principals),
				api.AIPrincipalsPage{Principals: principals})
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "user or api_key")
	cmd.Flags().StringVar(&source, "source", "", "source system, e.g. claude-code, codex")
	return cmd
}

func newPrincipalFindCommand() *cobra.Command {
	var kind string
	cmd := &cobra.Command{
		Use:   "find <name-or-id>",
		Short: "Find the principals matching a name, email, alias or id",
		Long: `Matches the way --user does: an exact id first, then an exact name or alias
(case-insensitive), then a unique substring. Several principals sharing one name
(the same person seen by two agents) count as one match.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validatePrincipalKind(kind); err != nil {
				return err
			}
			ctx, client, format, err := setupProjectCommand(cmd)
			if err != nil {
				return err
			}
			index, err := loadPrincipals(ctx, client)
			if err != nil {
				return err
			}
			matches, err := index.resolve(kind, args[0])
			if err != nil {
				return err
			}
			return output.Render(cmd.OutOrStdout(), format, principalTable(matches),
				api.AIPrincipalsPage{Principals: matches})
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "restrict to user or api_key")
	return cmd
}

func validatePrincipalKind(kind string) error {
	switch kind {
	case "", api.AIPrincipalUser, api.AIPrincipalAPIKey:
		return nil
	}
	return fmt.Errorf("--kind must be %s or %s", api.AIPrincipalUser, api.AIPrincipalAPIKey)
}

// principalFilter renders the catalog's AIP-160 filter: kind and source, ANDed.
func principalFilter(kind, source string) string {
	var terms []string
	if kind != "" {
		terms = append(terms, "kind="+aipQuote(kind))
	}
	if source != "" {
		terms = append(terms, "source_system="+aipQuote(source))
	}
	return strings.Join(terms, " AND ")
}

func principalTable(principals []api.AIPrincipal) output.Table {
	t := output.Table{Headers: []string{"SOURCE", "KIND", "NAME", "ALIAS", "ID"}}
	for _, p := range principals {
		t.Rows = append(t.Rows, []string{p.SourceSystem, p.Kind, p.Name, p.Alias, p.ID})
	}
	return t
}

func sortPrincipals(principals []api.AIPrincipal) {
	sort.SliceStable(principals, func(i, j int) bool {
		a, b := principals[i], principals[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if an, bn := strings.ToLower(a.DisplayName()), strings.ToLower(b.DisplayName()); an != bn {
			return an < bn
		}
		return a.SourceSystem < b.SourceSystem
	})
}

// principalIndex is the catalog loaded once per command: it labels ids in output and
// turns the names people type into the ids the filters need.
type principalIndex struct {
	all   []api.AIPrincipal
	byKey map[string]api.AIPrincipal
}

func principalKey(source, id string) string { return source + "\x00" + id }

func loadPrincipals(ctx context.Context, client *api.Client) (*principalIndex, error) {
	principals, err := client.AllAIPrincipals(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("load principal catalog: %w", err)
	}
	return newPrincipalIndex(principals), nil
}

func newPrincipalIndex(principals []api.AIPrincipal) *principalIndex {
	index := &principalIndex{all: principals, byKey: make(map[string]api.AIPrincipal, len(principals))}
	for _, p := range principals {
		index.byKey[principalKey(p.SourceSystem, p.ID)] = p
	}
	return index
}

// name labels an id for display; empty when the catalog has not seen it.
func (ix *principalIndex) name(source, id string) string {
	if ix == nil || id == "" {
		return ""
	}
	if p, ok := ix.byKey[principalKey(source, id)]; ok {
		return p.DisplayName()
	}
	return ""
}

// resolve maps what a person typed to principals. An exact id wins; then an exact
// name or alias, case-insensitively; then a unique substring of any of the three.
// One person is often several principals — the same email seen by Claude Code and by
// Codex — so a name match returns all of them. A substring that lands on different
// people is an error naming them, because silently picking one would attribute
// someone else's spend.
func (ix *principalIndex) resolve(kind, query string) ([]api.AIPrincipal, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("empty principal")
	}
	var pool []api.AIPrincipal
	for _, p := range ix.all {
		if kind == "" || p.Kind == kind {
			pool = append(pool, p)
		}
	}

	var byID []api.AIPrincipal
	for _, p := range pool {
		if p.ID == query {
			byID = append(byID, p)
		}
	}
	if len(byID) > 0 {
		return byID, nil
	}

	lower := strings.ToLower(query)
	var exact []api.AIPrincipal
	for _, p := range pool {
		if strings.EqualFold(p.Name, query) || strings.EqualFold(p.Alias, query) {
			exact = append(exact, p)
		}
	}
	if len(exact) > 0 {
		return exact, nil
	}

	// Substring: group the hits by who they are, so one person on two agents is one
	// candidate and two people are an ambiguity.
	groups := map[string][]api.AIPrincipal{}
	var order []string
	for _, p := range pool {
		if !strings.Contains(strings.ToLower(p.Name), lower) &&
			!strings.Contains(strings.ToLower(p.Alias), lower) &&
			!strings.Contains(strings.ToLower(p.ID), lower) {
			continue
		}
		who := strings.ToLower(p.DisplayName())
		if _, seen := groups[who]; !seen {
			order = append(order, who)
		}
		groups[who] = append(groups[who], p)
	}
	switch len(order) {
	case 1:
		return groups[order[0]], nil
	case 0:
		what := "principal"
		if kind != "" {
			what = kind
		}
		return nil, fmt.Errorf("no %s matches %q — run `terma principal list` to see who the project has seen", what, query)
	default:
		names := make([]string, 0, len(order))
		for _, who := range order {
			names = append(names, groups[who][0].DisplayName())
		}
		sort.Strings(names)
		return nil, fmt.Errorf("%q matches several principals (%s) — use the full name or the id", query, strings.Join(names, ", "))
	}
}

// resolveIDs is resolve for a list of flag values, flattening to the ids a filter takes.
func (ix *principalIndex) resolveIDs(kind string, queries []string) ([]string, error) {
	var ids []string
	for _, q := range queries {
		matches, err := ix.resolve(kind, q)
		if err != nil {
			return nil, err
		}
		for _, p := range matches {
			ids = append(ids, p.ID)
		}
	}
	return dedupe(ids), nil
}
