package cmd

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/miradorlabs/terma-cli/internal/api"
	"github.com/miradorlabs/terma-cli/internal/output"
)

func newSessionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "session",
		Aliases: []string{"sessions"},
		Short:   "List and inspect the coding sessions your agents ran",
		Long: `A session is one conversation with a coding agent — Claude Code, Codex, or another
connected harness — rolled up with its token usage, cost, tool calls and git activity.

A session is identified by its session id together with its source system, so
` + "`get`, `events` and `git`" + ` take both. Copy them from ` + "`session list`" + `.

--user and --api-key accept a name (an email, a key label, an alias set in the web
app) as well as an id; names are resolved through ` + "`terma principal`" + `.`,
	}
	cmd.AddCommand(
		newSessionListCommand(),
		newSessionGetCommand(),
		newSessionEventsCommand(),
		newSessionGitCommand(),
	)
	return cmd
}

// sessionSelectFlags are the who/what flags shared by session list and usage.
type sessionSelectFlags struct {
	sources, users, apiKeys, models, providers []string
	filter                                     string
}

func (f *sessionSelectFlags) bind(fl *pflag.FlagSet, withFilter bool) {
	fl.StringSliceVar(&f.sources, "source", nil, "source system (repeatable): claude-code, codex, …")
	fl.StringSliceVar(&f.users, "user", nil, "user by name, email, alias or id (repeatable)")
	fl.StringSliceVar(&f.apiKeys, "api-key", nil, "API key by label, alias or id (repeatable; never the secret)")
	fl.StringSliceVar(&f.models, "model", nil, "sessions that used this model (repeatable)")
	fl.StringSliceVar(&f.providers, "provider", nil, "model provider (repeatable)")
	if withFilter {
		fl.StringVarP(&f.filter, "filter", "f", "", "extra AIP-160 expression over source_system, user_id, api_key_id, model, provider")
	}
}

// selector resolves the names in the flags to ids through the catalog.
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

// sessionView is a session with its principal ids labelled, so a JSON consumer does
// not have to make a second call to learn who "u_1f3a" is.
type sessionView struct {
	api.AISession
	UserName   string `json:"user_name,omitempty"`
	APIKeyName string `json:"api_key_name,omitempty"`
}

// sessionListView is what `session list` renders. Pagination is the gateway's own,
// so it is there only when the rows are one of the gateway's pages: a walk (--all,
// --until) gathers rows across pages and has none to report.
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

// who is the one column a table has for identity: the person, else the key.
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

// defaultSessionPageSize is the gateway's page size when --page-size is not given. A
// walk that fills one page client-side stops at the same number.
const defaultSessionPageSize = 100

func newSessionListCommand() *cobra.Command {
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
			// A page number names one of the server's pages, and --until drops rows
			// from them, so "page 2" of the filtered list is not something either side
			// can address.
			if until != "" && page != 0 {
				return fmt.Errorf("--until is applied after the server pages the list, so it cannot be combined with --page; use --all, or a larger --page-size")
			}
			window, err := resolveWindow(since, until, 0, time.Now())
			if err != nil {
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
				// limit is how many rows fill a page of ours; zero is every row.
				limit := pageSize
				switch {
				case all:
					limit = 0
				case limit == 0:
					limit = defaultSessionPageSize
				}
				full := false
				err = client.ForEachAISession(ctx, q, func(s api.AISession) bool {
					// A session that reports no last activity cannot be placed before
					// --until, so it is left out rather than guessed at.
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
	sel.bind(cmd.Flags(), true)
	cmd.Flags().StringVar(&since, "since", "", "sessions last active at or after: RFC 3339, a date, a relative age (24h, 7d), today, yesterday")
	cmd.Flags().StringVar(&until, "until", "", "sessions last active before (same forms; applied client-side)")
	cmd.Flags().StringVar(&sortBy, "sort", "", "rank by "+strings.Join(api.AISessionSorts, ", ")+", highest first (default recency)")
	cmd.Flags().IntVar(&pageSize, "page-size", 0, "sessions per page, 1–1000 (default 100)")
	cmd.Flags().IntVar(&page, "page", 0, "page number, counting from 1 (default 1)")
	cmd.Flags().BoolVar(&all, "all", false, "follow every page")
	cmd.Flags().BoolVar(&follow, "follow", false, "tail the live catalog until interrupted")
	return cmd
}

// sessionIdentityFlag adds the --source flag every session-scoped read needs.
func sessionIdentityFlag(cmd *cobra.Command, source *string) {
	cmd.Flags().StringVar(source, "source", "", "the session's source system, e.g. claude-code (required)")
	_ = cmd.MarkFlagRequired("source")
}

// sessionNotFound names the fix for a 404 on a session-scoped read.
func sessionNotFound(source, id string) error {
	return fmt.Errorf("no %s session %q in this project — copy the session id and source from `terma session list`", source, id)
}

// sessionGetWait bounds `session get`. The gateway serves a single session's roll-up
// only as a live feed, which has no request timeout; a variable so a test can shorten it.
var sessionGetWait = 15 * time.Second

func newSessionGetCommand() *cobra.Command {
	var source string
	cmd := &cobra.Command{
		Use:     "get <session-id>",
		Aliases: []string{"show"},
		Short:   "Show one session's roll-up",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, client, format, err := setupProjectCommand(cmd)
			if err != nil {
				return err
			}
			index, err := loadPrincipals(ctx, client)
			if err != nil {
				return err
			}
			s, err := client.GetAISession(ctx, args[0], source, sessionGetWait)
			if err != nil {
				if api.IsNotFound(err) {
					return sessionNotFound(source, args[0])
				}
				return err
			}
			v := viewSession(s, index)
			pairs := [][2]string{
				{"session id", s.SessionID},
				{"source", s.SourceSystem},
				{"user", labelWithID(v.UserName, s.UserID)},
				{"api key", labelWithID(v.APIKeyName, s.APIKeyID)},
				{"started", localStamp(s.FirstSessionTime)},
				{"last active", localStamp(s.LastActivityAt)},
				{"turns", strconv.FormatUint(s.Turns, 10)},
				{"model calls", strconv.FormatUint(s.ModelCalls, 10)},
				{"tool calls", strconv.FormatUint(s.ToolCalls, 10)},
				{"user messages", strconv.FormatUint(s.UserMessages, 10)},
				{"tokens", usageBreakdown(s.Usage)},
				{"cost usd", money(s.Usage.CostUSD())},
				{"models", strings.Join(s.Models, ", ")},
				{"providers", strings.Join(s.Providers, ", ")},
				{"git", fmt.Sprintf("%d commits, %d pushes, %d PRs opened, %d merged, %d files touched",
					s.Commits, s.GitPushes, s.PullRequestsCreated, s.PullRequestsMerged, s.FilesTouched)},
				{"repositories", strings.Join(s.Repositories, ", ")},
			}
			return output.KeyValues(cmd.OutOrStdout(), format, pairs, v)
		},
	}
	sessionIdentityFlag(cmd, &source)
	return cmd
}

func labelWithID(name, id string) string {
	switch {
	case id == "":
		return "—"
	case name == "":
		return id
	default:
		return name + " (" + id + ")"
	}
}

func usageBreakdown(u *api.AITokenUsage) string {
	if u == nil {
		return "—"
	}
	return fmt.Sprintf("%d total (input %d, output %d, cache read %d, cache write %d)",
		u.TotalTokens(), u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
}

var toolEventKinds = []string{api.AIEventToolCall, api.AIEventToolResult, api.AIEventToolDecision}

func newSessionEventsCommand() *cobra.Command {
	var (
		source, tool, since, until string
		kinds                      []string
		toolsOnly, errorsOnly      bool
		contentWidth               int
	)
	cmd := &cobra.Command{
		Use:     "events <session-id>",
		Aliases: []string{"replay"},
		Short:   "Replay one session's events in conversational order",
		Long: `Prints a session's events — prompts, model calls with their usage, tool calls and
results, compactions, errors — oldest first. Content is whatever the harness was
connected to export; a harness connected with --exclude-prompts carries none.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, k := range kinds {
				if !slices.Contains(api.AIEventKinds, k) {
					return fmt.Errorf("unknown event kind %q (want one of %s)", k, strings.Join(api.AIEventKinds, ", "))
				}
			}
			window, err := resolveWindow(since, until, 0, time.Now())
			if err != nil {
				return err
			}
			ctx, client, format, err := setupProjectCommand(cmd)
			if err != nil {
				return err
			}
			// The gateway is sent the window, and the same half-open check stays in the
			// loop below: a gateway that does not know a parameter ignores it without
			// saying so.
			q := api.AISessionEventQuery{StartTime: window.since}
			if until != "" {
				q.EndTime = window.until
			}
			resp, err := client.AllAISessionEvents(ctx, args[0], source, q)
			if err != nil {
				if api.IsNotFound(err) {
					return sessionNotFound(source, args[0])
				}
				return err
			}

			kept := make([]api.AISessionEvent, 0, len(resp.Events))
			table := output.Table{Headers: []string{"TIME", "KIND", "TOOL / MODEL", "STATUS", "TOKENS", "COST USD", "CONTENT"}}
			for _, e := range resp.Events {
				switch {
				case len(kinds) > 0 && !slices.Contains(kinds, e.Kind),
					tool != "" && e.ToolName != tool,
					toolsOnly && !slices.Contains(toolEventKinds, e.Kind),
					errorsOnly && e.Kind != api.AIEventError && e.Status != "error" && e.Status != "failed",
					!window.since.IsZero() && e.EventTime.Before(window.since),
					until != "" && !e.EventTime.Before(window.until):
					continue
				}
				kept = append(kept, e)
				label := e.ToolName
				if label == "" {
					label = e.Model
				}
				t := e.EventTime
				table.Rows = append(table.Rows, []string{
					t.Local().Format("15:04:05"), e.Kind, label, e.Status,
					strconv.FormatUint(e.Usage.TotalTokens(), 10), money(e.Usage.CostUSD()),
					eventContent(e, contentWidth),
				})
			}
			resp.Events = kept
			return output.Render(cmd.OutOrStdout(), format, table, resp)
		},
	}
	sessionIdentityFlag(cmd, &source)
	fl := cmd.Flags()
	fl.StringSliceVar(&kinds, "kind", nil, "keep only these kinds (repeatable): "+strings.Join(api.AIEventKinds, ", "))
	fl.StringVar(&tool, "tool", "", "keep only events of this tool")
	fl.BoolVar(&toolsOnly, "tools-only", false, "keep tool calls, results and decisions")
	fl.BoolVar(&errorsOnly, "errors-only", false, "keep errors and failed events")
	fl.StringVar(&since, "since", "", "events at or after this time")
	fl.StringVar(&until, "until", "", "events before this time")
	fl.IntVar(&contentWidth, "content-width", 80, "characters of content per row in the table (0 = all)")
	return cmd
}

// eventContent flattens an event's parts for a table cell: parts in order, newlines
// folded to spaces so one event stays on one row.
func eventContent(e api.AISessionEvent, width int) string {
	parts := slices.Clone(e.Content)
	slices.SortStableFunc(parts, func(a, b api.AIContentPart) int {
		return int(a.PartIndex - b.PartIndex)
	})
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		texts = append(texts, strings.Join(strings.Fields(p.Content), " "))
	}
	joined := strings.Join(texts, " | ")
	if width > 0 {
		joined = output.Truncate(joined, width)
	}
	return joined
}

func gitTable(activities []api.AIGitActivity) output.Table {
	t := output.Table{Headers: []string{"TIME", "ACTION", "OUTCOME", "REPOSITORY", "REF", "COMMIT / PR", "MESSAGE"}}
	for _, a := range activities {
		t.Rows = append(t.Rows, gitRow(a))
	}
	return t
}

func gitRow(a api.AIGitActivity) []string {
	at := a.EventTime
	return []string{
		localStamp(&at), a.Action, a.Outcome, a.RepositoryKey, a.DestRef,
		output.Truncate(a.CommitRef(), 48), output.Truncate(a.MessageHeadline, 60),
	}
}

func newSessionGitCommand() *cobra.Command {
	var source string
	var follow bool
	cmd := &cobra.Command{
		Use:   "git <session-id>",
		Short: "Show the git and GitHub actions a session performed",
		Long: `Lists the branches, commits, pushes and pull requests an agent's tool calls
produced during a session, as evidence of actions — not a deduplicated commit
ledger. --follow tails the live feed; each frame replaces the earlier one with the
same activity_id as the evidence planes coalesce.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, client, format, err := setupProjectCommand(cmd)
			if err != nil {
				return err
			}
			if follow {
				stream, err := client.StreamAIGitActivities(ctx, args[0], source)
				if err != nil {
					return err
				}
				return followUpserts(cmd, format, stream, func(data json.RawMessage) error {
					var frame struct {
						Activity api.AIGitActivity `json:"activity"`
					}
					if err := json.Unmarshal(data, &frame); err != nil {
						return fmt.Errorf("decode git upsert: %w", err)
					}
					row := gitRow(frame.Activity)
					for i := range row {
						row[i] = output.SanitizeTerminal(row[i])
					}
					_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s  %-14s  %-10s  %s  %s  %s  %s\n",
						row[0], row[1], row[2], row[3], row[4], row[5], row[6])
					return err
				})
			}
			resp, err := client.ListAIGitActivities(ctx, args[0], source)
			if err != nil {
				if api.IsNotFound(err) {
					return sessionNotFound(source, args[0])
				}
				return err
			}
			return output.Render(cmd.OutOrStdout(), format, gitTable(resp.Activities), resp)
		},
	}
	sessionIdentityFlag(cmd, &source)
	cmd.Flags().BoolVar(&follow, "follow", false, "tail live updates until interrupted")
	return cmd
}
