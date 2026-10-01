package cli

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

func (app *App) newSessionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "session",
		Aliases: []string{"sessions"},
		Short:   "List and inspect the coding sessions your agents ran",
		Long: `A session is one conversation with a coding agent, rolled up with its token usage,
cost, tool calls and git activity.

A session is identified by its session id together with its source system, so
` + "`get`, `events` and `git`" + ` take both. Copy them from ` + "`session list`" + `.

--user and --api-key accept a name (an email, a key label, an alias set in the web
app) as well as an id; names are resolved through ` + "`terma principal`" + `.`,
	}
	cmd.AddCommand(
		app.newSessionListCommand(),
		app.newSessionGetCommand(),
		app.newSessionEventsCommand(),
		app.newSessionGitCommand(),
	)
	return cmd
}

// sessionSelectFlags are the who/what flags shared by session list and usage.
type sessionSelectFlags struct {
	sources, users, apiKeys, models, providers []string
	filter                                     string
}

func (f *sessionSelectFlags) bind(fl *pflag.FlagSet, withFilter bool, sources string) {
	fl.StringSliceVar(&f.sources, "source", nil, "source system (repeatable): "+sources+", …")
	fl.StringSliceVar(&f.users, "user", nil, "user by name, email, alias or id (repeatable)")
	fl.StringSliceVar(&f.apiKeys, "api-key", nil, "API key by label, alias or id (repeatable; never the secret)")
	fl.StringSliceVar(&f.models, "model", nil, "sessions that used this model (repeatable)")
	fl.StringSliceVar(&f.providers, "provider", nil, "model provider (repeatable)")
	if withFilter {
		fl.StringVarP(&f.filter, "filter", "f", "", "extra AIP-160 expression over source_system, user_id, api_key_id, model, provider")
	}
}

func (f *sessionSelectFlags) selector(index *principalIndex) (sessionSelector, error) {
	userIDs, err := index.resolveIDs(api.AIPrincipalUser, f.users)
	if err != nil {
		return sessionSelector{}, err
	}
	keyIDs, err := index.resolveIDs(api.AIPrincipalAPIKey, f.apiKeys)
	if err != nil {
		return sessionSelector{}, err
	}
	return sessionSelector{
		sources:   f.sources,
		userIDs:   userIDs,
		apiKeyIDs: keyIDs,
		models:    f.models,
		providers: f.providers,
		extra:     f.filter,
	}, nil
}

// sessionView is a session with its principal ids labelled, saving a JSON consumer a lookup.
type sessionView struct {
	api.AISession
	UserName   string `json:"user_name,omitempty"`
	APIKeyName string `json:"api_key_name,omitempty"`
}

// sessionListView is what `session list` renders; a walk across pages (--all, --until) has
// no gateway pagination to report.
type sessionListView struct {
	Sessions   []sessionView     `json:"sessions"`
	Pagination *api.AIPagination `json:"pagination,omitempty"`
	ProjectID  string            `json:"project_id,omitempty"`
}

func viewSession(s api.AISession, index *principalIndex) sessionView {
	return sessionView{
		AISession:  s,
		UserName:   index.name(s.SourceSystem, s.UserID),
		APIKeyName: index.name(s.SourceSystem, s.APIKeyID),
	}
}

func (v sessionView) who() string {
	switch {
	case v.UserName != "":
		return v.UserName
	case v.UserID != "":
		return v.UserID
	case v.APIKeyName != "":
		return v.APIKeyName
	default:
		return v.APIKeyID
	}
}

func sessionTable(sessions []sessionView) output.Table {
	t := output.Table{Headers: []string{"SOURCE", "USER", "STARTED", "LAST ACTIVE", "TURNS", "TOOLS", "TOKENS", "COST USD", "MODELS", "SESSION ID"}}
	for _, s := range sessions {
		t.Rows = append(t.Rows, []string{
			s.SourceSystem,
			output.Truncate(s.who(), 32),
			localStamp(s.FirstSessionTime),
			localStamp(s.LastActivityAt),
			strconv.FormatUint(s.Turns, 10),
			strconv.FormatUint(s.ToolCalls, 10),
			strconv.FormatUint(s.Usage.TotalTokens(), 10),
			money(s.Usage.CostUSD()),
			output.Truncate(strings.Join(s.Models, ","), 40),
			s.SessionID,
		})
	}
	return t
}

func money(usd float64) string { return fmt.Sprintf("%.4f", usd) }

// defaultSessionPageSize is the gateway's default page size, also used by client-side walks.
const defaultSessionPageSize = 100

func (app *App) newSessionListCommand() *cobra.Command {
	var (
		sel            sessionSelectFlags
		since, until   string
		sortBy         string
		page, pageSize int
		all, follow    bool
	)
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List sessions, most recently active first",
		Long: `Lists the project's sessions with their lifetime usage, most recently active first
unless --sort ranks them otherwise.

--since/--until select by a session's last activity, not by when it started: a
session that began last week and ran this morning is inside --since today. --since
is applied by the server. The server has no upper bound, so --until is applied
here, to the same field, reading as many pages as it takes to fill one. For spend
inside a calendar window use ` + "`terma usage`" + `.

One page by default. --page asks for a later one and --all follows every page.
--follow tails the live catalog instead: the first page, and each session on it
that is new or has changed, as it happens.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if pageSize < 0 || pageSize > 1000 {
				return fmt.Errorf("--page-size must be between 1 and 1000")
			}
			if page < 0 {
				return fmt.Errorf("--page counts from 1")
			}
			if sortBy != "" && !slices.Contains(api.AISessionSorts, sortBy) {
				return fmt.Errorf("unknown --sort %q (want one of %s)", sortBy, strings.Join(api.AISessionSorts, ", "))
			}
			if follow && (all || page != 0 || until != "") {
				return fmt.Errorf("--follow streams the first page of the catalog; it cannot be combined with --all, --page or --until")
			}
			// --until filters the server's pages, so a page of the filtered list is unaddressable.
			if until != "" && page != 0 {
				return fmt.Errorf("--until is applied after the server pages the list, so it cannot be combined with --page; use --all, or a larger --page-size")
			}
			window, err := resolveWindow(since, until, 0, time.Now())
			if err != nil {
				return err
			}
			ctx, client, format, err := app.setupProjectCommand(cmd)
			if err != nil {
				return err
			}
			index, err := loadPrincipals(ctx, client)
			if err != nil {
				return err
			}
			selector, err := sel.selector(index)
			if err != nil {
				return err
			}
			q := api.AISessionQuery{Filter: selector.filter(), Sort: sortBy, Page: page, PerPage: pageSize}
			if !window.since.IsZero() {
				q.ActiveAfter = rfc3339(window.since)
			}

			if follow {
				stream, err := client.StreamAISessions(ctx, q)
				if err != nil {
					return err
				}
				return followSessionSnapshots(cmd, format, stream, index)
			}

			view := sessionListView{Sessions: []sessionView{}}
			if all || until != "" {
				limit := pageSize
				switch {
				case all:
					limit = 0
				case limit == 0:
					limit = defaultSessionPageSize
				}
				full := false
				err = client.ForEachAISession(ctx, q, func(s api.AISession) bool {
					// A session with no last activity cannot be placed before --until.
					if until != "" && (s.LastActivityAt == nil || !s.LastActivityAt.Before(window.until)) {
						return true
					}
					view.Sessions = append(view.Sessions, viewSession(s, index))
					full = len(view.Sessions) == limit
					return !full
				})
				if err != nil {
					return err
				}
				if format == output.FormatTable && full {
					fmt.Fprintf(cmd.ErrOrStderr(), "Stopped at %d sessions; there may be more: rerun with --all, or a larger --page-size\n", limit)
				}
			} else {
				listed, err := client.ListAISessions(ctx, q)
				if err != nil {
					return err
				}
				view.ProjectID, view.Pagination = listed.ProjectID, &listed.Pagination
				for _, s := range listed.Sessions {
					view.Sessions = append(view.Sessions, viewSession(s, index))
				}
				if at := listed.Pagination; format == output.FormatTable && at.Page < at.TotalPages {
					fmt.Fprintf(cmd.ErrOrStderr(), "Page %d of %d (%d sessions): rerun with --all, or --page %d\n", at.Page, at.TotalPages, at.Total, at.Page+1)
				}
			}
			return output.Render(cmd.OutOrStdout(), format, sessionTable(view.Sessions), view)
		},
	}
	sel.bind(cmd.Flags(), true, app.sourceExamples())
	cmd.Flags().StringVar(&since, "since", "", "sessions last active at or after: RFC 3339, a date, a relative age (24h, 7d), today, yesterday")
	cmd.Flags().StringVar(&until, "until", "", "sessions last active before (same forms; applied client-side)")
	cmd.Flags().StringVar(&sortBy, "sort", "", "rank by "+strings.Join(api.AISessionSorts, ", ")+", highest first (default recency)")
	cmd.Flags().IntVar(&pageSize, "page-size", 0, "sessions per page, 1–1000 (default 100)")
	cmd.Flags().IntVar(&page, "page", 0, "page number, counting from 1 (default 1)")
	cmd.Flags().BoolVar(&all, "all", false, "follow every page")
	cmd.Flags().BoolVar(&follow, "follow", false, "tail the live catalog until interrupted")
	return cmd
}

func (app *App) sessionIdentityFlag(cmd *cobra.Command, source *string) {
	cmd.Flags().StringVar(source, "source", "", "the session's source system, e.g. "+app.sourceExamples()+" (required)")
	_ = cmd.MarkFlagRequired("source")
}

func sessionNotFound(source, id string) error {
	return fmt.Errorf("no %s session %q in this project — copy the session id and source from `terma session list`", source, id)
}
