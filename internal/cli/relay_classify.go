package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
)

// newRelayClassifyCommand says what the relay's content policy does with attribute keys, as
// this terma's relay would: the e2e suite's field catalog classifies every key an agent sends
// with it. Keys come as a JSON array on stdin ("resource/" prefixes a resource attribute's,
// "event/<name>/" one on a span event);
// the answer is a JSON object of key to safe, prompt, tool_content or unclassified.
func (app *App) newRelayClassifyCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "classify",
		Short:  "Classify attribute keys as the relay's content policy treats them",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			data, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return err
			}
			var keys []string
			if err := json.Unmarshal(data, &keys); err != nil {
				return fmt.Errorf("want a JSON array of attribute keys on stdin: %w", err)
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(relay.Classify(app.agents.With[shape.Capturer](), keys))
		},
	}
}
