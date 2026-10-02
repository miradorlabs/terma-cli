package connect

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// printConnectPlan always prints the redaction lines: "off" is the answer a reader wants.
func printConnectPlan(
	out io.Writer,
	h harness.Harness,
	cfg *config.Config,
	detection harness.Detection,
	configPath, helperPath string,
	signals []harness.Signal,
	reach harness.Reach,
	o Options,
) {
	printDetection(out, h, detection)
	fmt.Fprintf(out, "  Terma team: %s\n", cmp.Or(cfg.ProjectName, cfg.ProjectID))
	fmt.Fprintf(out, "  Endpoint:        %s\n", cfg.OTLPURL)

	if reach == harness.ReachRepos {
		// Three empty checkboxes would read as a mistake rather than as the arrangement asked for.
		fmt.Fprintf(out, "  Exports from:    repositories that carry a terma policy — every exporter here is left off\n")
		fmt.Fprintln(out, "\n  Signals:")
		fmt.Fprintln(out, "    decided by each repository's committed terma policy")
	} else {
		printDataPlan(out, signals)
	}

	fmt.Fprintln(out, "\n  This will update:")
	fmt.Fprintf(out, "    %s\n", configPath)
	if helperPath != "" {
		fmt.Fprintf(out, "    %s  (holds the key; the settings file will not)\n", helperPath)
	} else {
		fmt.Fprintln(out, "    (the server key is written into this file, which is tightened to 0600)")
	}
	if o.SuppliedKey == "" {
		// Reuse is decided after the plan, so the plan states the rule.
		fmt.Fprintln(out, "\n  A server key will be minted for this team — unless one is already installed here, which will be reused.")
	} else {
		fmt.Fprintf(out, "\n  Installing the key you supplied (%s).\n", harness.MaskKey(o.SuppliedKey))
	}
	fmt.Fprintln(out)
}

func printConnectNotes(out io.Writer, h harness.Harness, e harness.Exporter) {
	notes := h.ConnectNotes(e)
	if len(notes) == 0 {
		return
	}
	fmt.Fprintln(out, "  Note:")
	for _, note := range notes {
		fmt.Fprintf(out, "    %s\n", output.SanitizeTerminal(note))
	}
	fmt.Fprintln(out)
}

// resourceAttributes takes identity "none" to omit enduser.id, whose default is a real
// email written into a global config file.
func resourceAttributes(ctx context.Context, h harness.Harness, cfg *config.Config, identity string) map[string]string {
	attrs := map[string]string{
		harness.AttrServiceName: harness.ServiceName(h),
		harness.AttrProjectID:   cfg.ProjectID,
	}

	switch identity = strings.TrimSpace(identity); identity {
	case "none":
	case "":
		// git's global email: a repository-local one would follow the user out of its repo.
		if email := harness.GitEmail(ctx); email != "" {
			attrs[harness.AttrEnduserID] = email
		}
	default:
		attrs[harness.AttrEnduserID] = identity
	}
	return attrs
}

// PrintConflicts never prints a header value (it may hold someone else's credential) and
// sanitizes every field, since a key can carry a file name.
func PrintConflicts(out io.Writer, conflicts []harness.Conflict, force bool) {
	blocking, advisory := harness.Partition(conflicts)

	if len(blocking) > 0 {
		if force {
			fmt.Fprintln(out, "  Conflicting settings, which --force will remove where it can:")
		} else {
			fmt.Fprintln(out, "  Conflicting settings:")
		}
		for _, c := range blocking {
			printConflict(out, c)
		}
		fmt.Fprintln(out)
	}
	if len(advisory) > 0 {
		fmt.Fprintln(out, "  Overrides that apply only in a mode you select explicitly — reported, not blocking:")
		for _, c := range advisory {
			printConflict(out, c)
		}
		fmt.Fprintln(out)
	}
}

func printConflict(out io.Writer, c harness.Conflict) {
	key := output.SanitizeTerminal(c.Key)
	if c.Value != "" {
		fmt.Fprintf(out, "    %s=%s\n", key, output.SanitizeTerminal(c.Value))
	} else {
		fmt.Fprintf(out, "    %s\n", key)
	}
	marker := " "
	if c.Credential {
		marker = "!"
	}
	fmt.Fprintf(out, "     %s [%s] %s\n", marker, output.SanitizeTerminal(c.Scope), output.SanitizeTerminal(c.Reason))
	if !c.Clearable && !c.Advisory {
		// Said here and in the error: the user has to fix this one themselves.
		if c.Scope == harness.ScopeUserSettings {
			fmt.Fprintf(out, "       Terma does not change this — it is your setting to remove.\n")
		} else {
			fmt.Fprintf(out, "       Terma cannot change this — it is outside the file Terma writes.\n")
		}
	}
}

func unclearable(conflicts []harness.Conflict) []string {
	var out []string
	for _, c := range conflicts {
		if !c.Clearable {
			out = append(out, c.Key)
		}
	}
	return out
}

// InstallStatusLine wraps c's status line and says how; it only warns on failure, since
// the exporters are already written by then.
func InstallStatusLine(c agents.StatusLiner, errOut io.Writer) (string, bool) {
	changed, err := c.InstallStatusLine()
	if err != nil {
		fmt.Fprintf(errOut, "Warning: could not wrap %s's status line (%v); plan usage will not be captured.\n", c.DisplayName(), err)
		return "", false
	}
	st, stErr := c.StatusLineState("")
	switch {
	case stErr != nil:
		return "", true
	case changed && st.Renderer != "":
		return fmt.Sprintf("Status line: terma now reads the plan's usage windows from it; your own status line (%s) keeps running unchanged behind it.", output.SanitizeTerminal(st.Renderer)), true
	case changed:
		return "Status line: terma added one that shows model, context, cost and the plan's usage windows (remove it with `terma disconnect " + c.Name() + "`, or skip it with --no-statusline).", true
	default:
		return "Status line: already wrapped by terma.", true
	}
}

// JoinSignals names signals for prose.
func JoinSignals(signals []harness.Signal) string {
	parts := make([]string, 0, len(signals))
	for _, s := range signals {
		parts = append(parts, string(s))
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

// SignalLabel is how a plan names a signal.
func SignalLabel(s harness.Signal) string {
	switch s {
	case harness.SignalTraces:
		return "Agent traces"
	case harness.SignalLogs:
		return "Structured events"
	case harness.SignalMetrics:
		return "Token and cost metrics"
	default:
		return string(s)
	}
}

// printLocalConnectPlan names the global connect under the policy: with nothing under it
// the policy ships nothing, and the reader should learn that here.
func printLocalConnectPlan(
	out io.Writer,
	global harness.Harness,
	detection harness.Detection,
	cfg *config.Config,
	root, configPath string,
	e harness.Exporter,
) {
	printDetection(out, global, detection)
	fmt.Fprintf(out, "  Scope:       this repository (%s)\n", root)
	st, err := global.Status()
	if err == nil && st.Connected && st.Endpoint == cfg.OTLPURL {
		fmt.Fprintf(out, "  Exports via: your global %s connect (%s)\n", global.DisplayName(), st.Endpoint)
	} else {
		fmt.Fprintf(out, "  Exports via: nothing yet — %s is not connected to Terma on this machine.\n", global.DisplayName())
		fmt.Fprintf(out, "               This file decides what to ship; `terma connect %s` says where.\n", global.Name())
	}

	printDataPlan(out, e.Signals)

	fmt.Fprintln(out, "\n  This will update:")
	fmt.Fprintf(out, "    %s  (what to ship — the endpoint and key stay in your user settings)\n", configPath)
	fmt.Fprintln(out)
}

func printDetection(out io.Writer, h harness.Harness, detection harness.Detection) {
	if detection.Found {
		version := detection.Version
		if version == "" {
			version = "version unknown"
		}
		fmt.Fprintf(out, "%s found: %s\n", h.DisplayName(), version)
		return
	}
	// Not an error: the config is read whenever the agent is eventually started.
	fmt.Fprintf(out, "%s not found on PATH — the configuration will still be written.\n", h.DisplayName())
}

// printDataPlan names no content switch: what content leaves is the team policy's call.
func printDataPlan(out io.Writer, signals []harness.Signal) {
	fmt.Fprintln(out, "\n  Signals:")
	for _, s := range harness.AllSignals {
		mark := " "
		if slices.Contains(signals, s) {
			mark = "✓"
		}
		fmt.Fprintf(out, "    %s %s\n", mark, SignalLabel(s))
	}
}
