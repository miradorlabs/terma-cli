package cli

import (
	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/live"
)

func (app *App) newNotifyCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "notify",
		Short:  "Tell the developer about a live finding",
		Hidden: true,
		Long: `A hook that finds something the developer should know records it and exits; a
detached process tells them, so the agent never waits on it.`,
	}
	cmd.AddCommand(app.newNotifyAwaitAnswerCommand())
	return cmd
}

// newNotifyAwaitAnswerCommand is the detached half of the Notification hook: it nudges the
// developer once their agent has waited live.AnswerWait for them.
func (app *App) newNotifyAwaitAnswerCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "await-answer <record> <id>",
		Short:  "Internal: nudge the developer once an agent has waited for their answer",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if app.notifier != nil {
				live.AwaitAnswer(cmd.Context(), args[0], args[1], app.notifier)
			}
			return nil
		},
	}
}
