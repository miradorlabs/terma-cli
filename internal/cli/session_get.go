package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

func (app *App) newSessionGetCommand() *cobra.Command {
	var source string
	cmd := &cobra.Command{
		Use:     "get <session-id>",
		Aliases: []string{"show"},
		Short:   "Show one session's roll-up",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, client, format, err := app.setupProjectCommand(cmd)
			if err != nil {
				return err
			}
			index, err := loadPrincipals(ctx, client)
			if err != nil {
				return err
			}
			s, err := client.GetAISession(ctx, args[0], source, app.sessionGetWait)
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
	app.sessionIdentityFlag(cmd, &source)
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
