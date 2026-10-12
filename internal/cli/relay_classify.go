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
// with it. Keys come as a JSON array on stdin, each {"site": "record" | "resource" | "event",
// "event": <a span event's name>, "key": <the key>}; the answer is an array in the same order,
// each {"class": "safe" | "prompt" | "tool_content" | "unclassified", "kept": [the kinds of
// value the key keeps with all content withheld]}.
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
			var queries []relay.FieldQuery
			if err := json.Unmarshal(data, &queries); err != nil {
				return fmt.Errorf("want a JSON array of attribute keys on stdin: %w", err)
			}
			for _, q := range queries {
				if q.Site != relay.SiteRecord && q.Site != relay.SiteResource && q.Site != relay.SiteEvent {
					return fmt.Errorf("%q sits nowhere the relay knows: %q", q.Key, q.Site)
				}
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(relay.Classify(app.agents.With[shape.Capturer](), queries))
		},
	}
}
