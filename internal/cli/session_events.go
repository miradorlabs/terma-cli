package cli

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

var toolEventKinds = []string{api.AIEventToolCall, api.AIEventToolResult, api.AIEventToolDecision}

func (app *App) newSessionEventsCommand() *cobra.Command {
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
			ctx, client, format, err := app.setupProjectCommand(cmd)
			if err != nil {
				return err
			}
			// The loop rechecks the window: a gateway ignores a parameter it does not know.
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
	app.sessionIdentityFlag(cmd, &source)
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

// eventContent flattens an event's parts into one table row.
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

func (app *App) newSessionGitCommand() *cobra.Command {
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
			ctx, client, format, err := app.setupProjectCommand(cmd)
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
	app.sessionIdentityFlag(cmd, &source)
	cmd.Flags().BoolVar(&follow, "follow", false, "tail live updates until interrupted")
	return cmd
}
