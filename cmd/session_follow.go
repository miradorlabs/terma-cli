package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/api"
	"github.com/miradorlabs/terma-cli/internal/output"
)

// followFrames drains a live feed, handing every frame that carries state to onFrame.
// Heartbeats are swallowed, an `error` frame ends the command with the gateway's
// message, and the hourly connection rotation surfaces as an error rather than a
// silent stop.
func followFrames(cmd *cobra.Command, format output.Format, stream *api.Stream, onFrame func(*api.Event) error) error {
	if format != output.FormatTable && format != output.FormatJSON {
		return fmt.Errorf("--follow renders as table or json (newline-delimited events)")
	}
	defer func() { _ = stream.Close() }()
	ctx := cmd.Context()
	for {
		f, err := stream.Next()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("the server closed the stream (connections rotate hourly) — rerun to reconnect with a fresh snapshot")
			}
			return err
		}
		switch f.Name {
		case "heartbeat":
			continue
		case "error":
			return api.AIStreamError(f)
		}
		if err := onFrame(f); err != nil {
			return err
		}
	}
}

// writeFrame prints a frame the way --follow's JSON does: one newline-delimited
// envelope {"event": …, "data": …}, the data exactly as the gateway sent it.
func writeFrame(w io.Writer, f *api.Event) error {
	return json.NewEncoder(w).Encode(struct {
		Event string          `json:"event"`
		Data  json.RawMessage `json:"data"`
	}{f.Name, json.RawMessage(f.Data)})
}

// followUpserts drains a live upsert feed. A table prints one line per upsert as it
// arrives; JSON is one envelope per frame so a consumer can key on activity_id and
// replace.
func followUpserts(cmd *cobra.Command, format output.Format, stream *api.Stream, onUpsert func(json.RawMessage) error) error {
	return followFrames(cmd, format, stream, func(f *api.Event) error {
		switch {
		case f.Name != "upsert" && f.Name != "snapshot_completed":
			return nil
		case format == output.FormatJSON:
			return writeFrame(cmd.OutOrStdout(), f)
		case f.Name == "snapshot_completed":
			fmt.Fprintln(cmd.ErrOrStderr(), "Snapshot complete; following live updates (Ctrl-C to stop).")
			return nil
		}
		return onUpsert(json.RawMessage(f.Data))
	})
}

// followSessionSnapshots drains the live session catalog. The gateway does not send
// changes: it re-sends the whole first page on a fixed interval, changed or not. A
// frame identical to the last carries nothing and is dropped. JSON passes every other
// frame through for the consumer to replace its copy with; a table prints one line per
// session that is new or differs from the row last printed for it.
func followSessionSnapshots(cmd *cobra.Command, format output.Format, stream *api.Stream, index *principalIndex) error {
	var last string
	printed := map[string]string{}
	return followFrames(cmd, format, stream, func(f *api.Event) error {
		if f.Name != "snapshot" || f.Data == last {
			return nil
		}
		first := last == ""
		last = f.Data
		if format == output.FormatJSON {
			return writeFrame(cmd.OutOrStdout(), f)
		}
		var frame struct {
			Sessions []json.RawMessage `json:"sessions"`
		}
		if err := json.Unmarshal([]byte(f.Data), &frame); err != nil {
			return fmt.Errorf("decode session snapshot: %w", err)
		}
		for _, row := range frame.Sessions {
			var s api.AISession
			if err := json.Unmarshal(row, &s); err != nil {
				return fmt.Errorf("decode session snapshot: %w", err)
			}
			key := s.SourceSystem + "\x00" + s.SessionID
			if printed[key] == string(row) {
				continue
			}
			printed[key] = string(row)
			v := viewSession(s, index)
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s  %-12s  %-28s  %8d tokens  $%s  %s\n",
				localStamp(v.LastActivityAt), output.SanitizeTerminal(v.SourceSystem),
				output.SanitizeTerminal(output.Truncate(v.who(), 28)), v.Usage.TotalTokens(),
				money(v.Usage.CostUSD()), output.SanitizeTerminal(v.SessionID)); err != nil {
				return err
			}
		}
		if first {
			fmt.Fprintln(cmd.ErrOrStderr(), "Snapshot complete; following live updates (Ctrl-C to stop).")
		}
		return nil
	})
}
