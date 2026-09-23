package cmd

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/api"
	"github.com/miradorlabs/terma-cli/internal/output"
)

// usageGroups maps --group-by to the metric labels a row is keyed by. Principal ids
// are only meaningful with their source system — Claude Code's user ids and Codex's
// are different namespaces — so those groups carry it.
var usageGroups = map[string][]string{
	"user":     {"source_system", "user_id"},
	"api-key":  {"source_system", "api_key_id"},
	"source":   {"source_system"},
	"model":    {"model"},
	"provider": {"provider"},
	"none":     {},
}

var usageGroupNames = []string{"user", "api-key", "source", "model", "provider", "none"}

// usageMetrics are the terma.ai.* counters the platform derives from settled model
// calls. They share one label set — source_system, provider, model, user_id,
// api_key_id — so a single group-by applies to all of them. model_call.total also
// carries status, which the sum folds away.
var usageMetrics = []struct{ field, name string }{
	{"cost_usd", "terma.ai.cost.usd.total"},
	{"input_tokens", "terma.ai.tokens.input.total"},
	{"output_tokens", "terma.ai.tokens.output.total"},
	{"cache_read_tokens", "terma.ai.tokens.cache_read.total"},
	{"cache_write_tokens", "terma.ai.tokens.cache_write.total"},
	{"model_calls", "terma.ai.model_call.total"},
}

var usageLabelHeaders = map[string]string{
	"source_system": "SOURCE",
	"user_id":       "USER ID",
	"api_key_id":    "API KEY ID",
	"model":         "MODEL",
	"provider":      "PROVIDER",
}

// usageRow is one group's spend inside the window. Token and call counts are rounded
// to whole numbers: the counters are integers, and the small extrapolation
// increase() applies at the window edges is not a fraction of a token anyone wants.
type usageRow struct {
	Group            map[string]string `json:"group,omitempty"`
	Name             string            `json:"name,omitempty"`
	CostUSD          float64           `json:"cost_usd"`
	InputTokens      uint64            `json:"input_tokens"`
	OutputTokens     uint64            `json:"output_tokens"`
	CacheReadTokens  uint64            `json:"cache_read_tokens"`
	CacheWriteTokens uint64            `json:"cache_write_tokens"`
	TotalTokens      uint64            `json:"total_tokens"`
	ModelCalls       uint64            `json:"model_calls"`
}

func (r *usageRow) set(field string, v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		v = 0
	}
	n := uint64(math.Round(v))
	switch field {
	case "cost_usd":
		r.CostUSD = v
	case "input_tokens":
		r.InputTokens = n
	case "output_tokens":
		r.OutputTokens = n
	case "cache_read_tokens":
		r.CacheReadTokens = n
	case "cache_write_tokens":
		r.CacheWriteTokens = n
	case "model_calls":
		r.ModelCalls = n
	}
}

func (r *usageRow) add(o usageRow) {
	r.CostUSD += o.CostUSD
	r.InputTokens += o.InputTokens
	r.OutputTokens += o.OutputTokens
	r.CacheReadTokens += o.CacheReadTokens
	r.CacheWriteTokens += o.CacheWriteTokens
	r.ModelCalls += o.ModelCalls
	r.TotalTokens = r.InputTokens + r.OutputTokens + r.CacheReadTokens + r.CacheWriteTokens
}

type usageReport struct {
	// Basis says how the numbers were produced, so a consumer never has to guess:
	// metrics_window is spend that happened inside [since, until), whichever
	// session it belonged to.
	Basis   string              `json:"basis"`
	Since   time.Time           `json:"since"`
	Until   time.Time           `json:"until"`
	GroupBy string              `json:"group_by"`
	Filters map[string][]string `json:"filters,omitempty"`
	Rows    []usageRow          `json:"rows"`
	Totals  usageRow            `json:"totals"`
	Queries []string            `json:"queries"`
}

func newUsageCommand() *cobra.Command {
	var (
		sel          sessionSelectFlags
		since, until string
		groupBy      string
	)
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Sum token usage and cost over a time window, by person, agent or model",
		Long: `Answers "how much did <someone> use today?" from the platform's usage metrics: the
tokens and provider cost of every model call that settled inside the window,
whichever session it belonged to. Defaults to the last 24 hours grouped by user.

  terma usage --user dawson --since today
  terma usage --group-by model --since 7d
  terma usage --source codex --since yesterday --until today

Numbers come from counters sampled over time, so a window's edges are interpolated
to the nearest sample; for a per-session ledger use ` + "`terma session list`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			by, ok := usageGroups[groupBy]
			if !ok {
				return fmt.Errorf("--group-by must be one of %s", strings.Join(usageGroupNames, ", "))
			}
			window, err := resolveWindow(since, until, 24*time.Hour, time.Now())
			if err != nil {
				return err
			}
			span := window.until.Sub(window.since).Round(time.Second)
			if span < time.Second {
				return fmt.Errorf("the window from --since to --until is shorter than a second")
			}

			ctx, client, format, err := setupProjectCommand(cmd)
			if err != nil {
				return err
			}
			index, err := loadPrincipals(ctx, client)
			if err != nil {
				return err
			}
			userIDs, err := index.resolveIDs(api.AIPrincipalUser, sel.users)
			if err != nil {
				return err
			}
			keyIDs, err := index.resolveIDs(api.AIPrincipalAPIKey, sel.apiKeys)
			if err != nil {
				return err
			}
			filters := map[string][]string{}
			var matchers []string
			for _, m := range []struct {
				label  string
				values []string
			}{
				{"source_system", sel.sources},
				{"user_id", userIDs},
				{"api_key_id", keyIDs},
				{"model", sel.models},
				{"provider", sel.providers},
			} {
				if values := dedupe(m.values); len(values) > 0 {
					filters[m.label] = values
					matchers = append(matchers, promMatcher(m.label, values))
				}
			}

			report := usageReport{
				Basis: "metrics_window", Since: window.since.UTC(), Until: window.until.UTC(),
				GroupBy: groupBy, Filters: filters, Rows: []usageRow{},
			}
			rows := map[string]*usageRow{}
			var order []string
			for _, m := range usageMetrics {
				expr := usageQuery(m.name, by, matchers, span)
				report.Queries = append(report.Queries, expr)
				res, err := client.QueryMetric(ctx, expr, window.until)
				if err != nil {
					return fmt.Errorf("%s: %w", m.name, err)
				}
				for _, sample := range res.Data.Result {
					v, err := sample.Float()
					if err != nil {
						return fmt.Errorf("%s: bad sample %q: %w", m.name, sample.Value.Value, err)
					}
					key := usageKey(sample.Metric, by)
					row := rows[key]
					if row == nil {
						row = &usageRow{Group: usageGroup(sample.Metric, by)}
						rows[key] = row
						order = append(order, key)
					}
					row.set(m.field, v)
				}
			}

			for _, key := range order {
				r := rows[key]
				r.TotalTokens = r.InputTokens + r.OutputTokens + r.CacheReadTokens + r.CacheWriteTokens
				// A series that exists but did not move is a model or person that was idle
				// in this window. Listing it at zero would read as "used, cost nothing".
				if r.CostUSD == 0 && r.TotalTokens == 0 && r.ModelCalls == 0 {
					continue
				}
				switch groupBy {
				case "user":
					r.Name = index.name(r.Group["source_system"], r.Group["user_id"])
				case "api-key":
					r.Name = index.name(r.Group["source_system"], r.Group["api_key_id"])
				}
				report.Rows = append(report.Rows, *r)
				report.Totals.add(*r)
			}
			sort.SliceStable(report.Rows, func(i, j int) bool {
				a, b := report.Rows[i], report.Rows[j]
				if a.CostUSD != b.CostUSD {
					return a.CostUSD > b.CostUSD
				}
				if a.TotalTokens != b.TotalTokens {
					return a.TotalTokens > b.TotalTokens
				}
				return usageKey(a.Group, by) < usageKey(b.Group, by)
			})

			if format == output.FormatTable {
				fmt.Fprintf(cmd.ErrOrStderr(), "Usage from %s to %s\n",
					window.since.Local().Format("2006-01-02 15:04"), window.until.Local().Format("2006-01-02 15:04 MST"))
				if len(report.Rows) == 0 {
					fmt.Fprintln(cmd.ErrOrStderr(), "No usage recorded for this window and filter. Metrics come from settled model calls; `terma session list` shows the raw sessions.")
				}
			}
			return output.Render(cmd.OutOrStdout(), format, usageTable(report, by, groupBy), report)
		},
	}
	sel.bind(cmd.Flags(), false)
	cmd.Flags().StringVar(&since, "since", "", "window start: RFC 3339, a date, a relative age (24h, 7d), today, yesterday (default 24h)")
	cmd.Flags().StringVar(&until, "until", "", "window end (same forms; default now)")
	cmd.Flags().StringVar(&groupBy, "group-by", "user", strings.Join(usageGroupNames, ", "))
	return cmd
}

// usageQuery is the PromQL the web app's insights page runs, spelled out: the increase
// of one counter over the window, summed by the group labels. Metric names carry
// dots, so they are selected through __name__ rather than written bare.
func usageQuery(metric string, by, matchers []string, window time.Duration) string {
	selector := `__name__="` + metric + `"`
	if len(matchers) > 0 {
		selector += ", " + strings.Join(matchers, ", ")
	}
	inner := fmt.Sprintf("increase({%s}[%ds])", selector, int64(window.Seconds()))
	if len(by) == 0 {
		return "sum(" + inner + ")"
	}
	return "sum by (" + strings.Join(by, ", ") + ") (" + inner + ")"
}

func usageGroup(labels map[string]string, by []string) map[string]string {
	if len(by) == 0 {
		return nil
	}
	g := make(map[string]string, len(by))
	for _, l := range by {
		g[l] = labels[l]
	}
	return g
}

func usageKey(labels map[string]string, by []string) string {
	parts := make([]string, len(by))
	for i, l := range by {
		parts[i] = labels[l]
	}
	return strings.Join(parts, "\x00")
}

// usageTable lays the report out for a person. For the principal groupings the name
// leads and the id — a 64-hex digest nobody reads — is shortened; JSON keeps it whole.
func usageTable(report usageReport, by []string, groupBy string) output.Table {
	named := groupBy == "user" || groupBy == "api-key"
	var headers []string
	if named {
		headers = append(headers, "SOURCE", "NAME")
		for _, l := range by[1:] {
			headers = append(headers, usageLabelHeaders[l])
		}
	} else {
		for _, l := range by {
			headers = append(headers, usageLabelHeaders[l])
		}
	}
	headers = append(headers, "COST USD", "INPUT", "OUTPUT", "CACHE READ", "CACHE WRITE", "TOTAL TOKENS", "MODEL CALLS")

	row := func(r usageRow, first []string) []string {
		return append(first,
			money(r.CostUSD),
			strconv.FormatUint(r.InputTokens, 10), strconv.FormatUint(r.OutputTokens, 10),
			strconv.FormatUint(r.CacheReadTokens, 10), strconv.FormatUint(r.CacheWriteTokens, 10),
			strconv.FormatUint(r.TotalTokens, 10), strconv.FormatUint(r.ModelCalls, 10))
	}
	t := output.Table{Headers: headers}
	for _, r := range report.Rows {
		var first []string
		if named {
			id := r.Group[by[1]]
			name := r.Name
			switch {
			case id == "" && groupBy == "user":
				// Traffic attributed to an API key rather than a person (OpenRouter,
				// Cloudflare). Group by api-key to see which one.
				name = "(no user id)"
			case id == "":
				name = "(no key id)"
			case name == "":
				name = id
			}
			first = append(first, r.Group["source_system"], output.Truncate(name, 40), output.Truncate(id, 16))
		} else {
			for _, l := range by {
				first = append(first, r.Group[l])
			}
		}
		t.Rows = append(t.Rows, row(r, first))
	}
	if len(report.Rows) > 1 {
		first := make([]string, len(headers)-7)
		if len(first) > 0 {
			first[0] = "TOTAL"
		}
		t.Rows = append(t.Rows, row(report.Totals, first))
	}
	return t
}
