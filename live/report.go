package live

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The report is the artefact a person reads: which harness versions ran, with
// which credential class, which contracts passed, which were not run and why,
// and what the run cost. Tests add to it as they go; TestMain writes it.

type reportEntry struct {
	Test   string
	Status string // pass | fail | not run
	Note   string
}

var (
	reportMu    sync.Mutex
	reportRows  []reportEntry
	reportNotes = map[string][]string{}
	spendUSD    float64
)

// Record notes a test's outcome for the report. One row per test: a test that
// already explained itself keeps its explanation, and a failure overrides.
func Record(test, status, note string) {
	reportMu.Lock()
	defer reportMu.Unlock()
	for i, r := range reportRows {
		if r.Test != test {
			continue
		}
		if status == "fail" && r.Status != "fail" {
			reportRows[i] = reportEntry{test, status, note}
		}
		return
	}
	reportRows = append(reportRows, reportEntry{test, status, note})
}

// Note attaches an observation to a surface (golden drift, versions), once.
func Note(surface, note string) {
	reportMu.Lock()
	defer reportMu.Unlock()
	for _, n := range reportNotes[surface] {
		if n == note {
			return
		}
	}
	reportNotes[surface] = append(reportNotes[surface], note)
}

// AddSpend accumulates the harness-reported cost of a run.
func AddSpend(usd float64) {
	reportMu.Lock()
	defer reportMu.Unlock()
	spendUSD += usd
}

// WriteReport renders report/latest.md.
func WriteReport(dir string) error {
	reportMu.Lock()
	defer reportMu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "# Live collection report\n\n%s\n\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "| binary | version |\n|---|---|\n")
	for _, bin := range []string{"terma", "claude", "codex", "opencode"} {
		path := bin
		if bin == "terma" && os.Getenv("TERMA_LIVE_BINARY") != "" {
			path = os.Getenv("TERMA_LIVE_BINARY")
		}
		fmt.Fprintf(&b, "| %s | %s |\n", bin, Version(path))
	}
	fmt.Fprintf(&b, "\n| credential | present |\n|---|---|\n")
	for _, k := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_FEDERATION_RULE_ID", "OPENAI_API_KEY", "TERMA_LIVE_REAL_LOGIN", "TERMA_LIVE_CODEX_AUTH"} {
		present := "no"
		if os.Getenv(k) != "" {
			present = "yes"
		}
		fmt.Fprintf(&b, "| %s | %s |\n", k, present)
	}
	fmt.Fprintf(&b, "\n| test | status | note |\n|---|---|---|\n")
	for _, r := range reportRows {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", r.Test, r.Status, r.Note)
	}
	if len(reportNotes) > 0 {
		fmt.Fprintf(&b, "\n## Surfaces\n\n")
		names := make([]string, 0, len(reportNotes))
		for n := range reportNotes {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(&b, "- **%s**: %s\n", n, strings.Join(reportNotes[n], "; "))
		}
	}
	fmt.Fprintf(&b, "\nHarness-reported spend this run: $%.4f\n", spendUSD)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "latest.md"), []byte(b.String()), 0o644)
}
