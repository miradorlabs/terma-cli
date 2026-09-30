package live

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"
)

// The compatibility matrix: which harness builds terma works with, capability by
// capability, as the live suite proved it on real binaries. A scenario says what it
// proves (Proves); its outcome is recorded when it finishes; TestMain writes the run's
// rows to report/compat.json, which `make compat` merges into the history and renders
// as docs/COMPATIBILITY.md and docs/compat/compat.json (for the website).

// Capability is one thing terma promises for a harness.
type Capability struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Capabilities are the matrix's rows, in order. An ID is a contract with the history
// and the website: add one, never rename or reuse one.
var Capabilities = []Capability{
	{"relay.telemetry", "Telemetry reaches the project through the local relay"},
	{"relay.only_opted_in", "Nothing leaves for sessions no repository opted in to"},
	{"relay.content_withheld", "Prompts and tool content withheld when the project says so"},
	{"relay.content_allowed", "Prompts and tool content delivered when allowed"},
	{"relay.equivalent", "The same telemetry through the relay as exported directly"},
	{"relay.cold_start", "Relay not running when the session starts"},
	{"relay.late_claim", "Session claimed after its first export"},
	{"relay.concurrent_projects", "Two repositories' sessions at once, each to its own project"},
	{"relay.resumed_elsewhere", "A session resumed outside the repository stays out"},
	{"relay.subagents", "Subagents' work attributed to their session"},
	{"relay.tools_no_token", "Tools the agent runs never inherit the relay's token"},
	{"relay.desktop", "The desktop app through the relay"},
	{"relay.daemon", "The shared background server through the relay"},
	{"global.placement", "Global mode: every session placed in its project"},
	{"global.commits", "Global mode: every commit carries its session"},
	{"global.managed", "Global mode: organization-managed hooks, no trust step"},
	{"relay.heartbeat", "The machine's heartbeat reaches the organization"},
}

// Harnesses are the matrix's columns' groups, with the name docs show.
var Harnesses = []struct{ ID, Name string }{
	{"claude", "Claude Code"}, {"codex", "Codex CLI"}, {"opencode", "OpenCode"}, {"gemini", "Gemini CLI"},
	{"omp", "oh-my-pi (omp)"}, {"pi", "Pi"}, {"hermes", "Hermes"}, {"dsh", "DeepSeek Harness"},
	{"t3", "T3 Code"}, {"cursor", "Cursor"}, {"claude-desktop", "Claude Desktop"}, {"codex-desktop", "Codex Desktop"},
}

// CompatRow is one proven (or failed) capability for one build, from one run.
type CompatRow struct {
	Harness    string    `json:"harness"`
	Version    string    `json:"version"`
	Capability string    `json:"capability"`
	Result     string    `json:"result"` // pass | fail | not run
	Platform   string    `json:"platform"`
	Test       string    `json:"test"`
	Terma      string    `json:"terma,omitempty"`
	At         time.Time `json:"at"`
}

var (
	compatMu   sync.Mutex
	compatRows []CompatRow
)

// Proves records that t proves capability for harness at version: a pass when t
// passes, a failure when it fails, "not run" when it is skipped. Call it where the
// build is known, before the scenario's checks.
func Proves(t *testing.T, harness, version, capability string) {
	t.Helper()
	known := false
	for _, c := range Capabilities {
		known = known || c.ID == capability
	}
	if !known {
		t.Fatalf("Proves: unknown capability %q: add it to Capabilities", capability)
	}
	t.Cleanup(func() {
		result := "pass"
		switch {
		case t.Failed():
			result = "fail"
		case t.Skipped():
			result = "not run"
		}
		compatMu.Lock()
		defer compatMu.Unlock()
		compatRows = append(compatRows, CompatRow{Harness: harness, Version: version, Capability: capability,
			Result: result, Platform: runtime.GOOS + "/" + runtime.GOARCH, Test: t.Name(), At: time.Now().UTC()})
	})
}

// ProvesAll is Proves for several capabilities of one build.
func ProvesAll(t *testing.T, b Binary, capabilities ...string) {
	t.Helper()
	for _, c := range capabilities {
		Proves(t, b.Harness, b.Version, c)
	}
}

// WriteCompat writes this run's rows to dir/compat.json. A capability proven by several
// tests is failed by any of them.
func WriteCompat(dir, terma string) error {
	compatMu.Lock()
	rows := append([]CompatRow(nil), compatRows...)
	compatMu.Unlock()
	merged := map[[4]string]CompatRow{}
	for _, r := range rows {
		r.Terma = terma
		k := [4]string{r.Harness, r.Version, r.Platform, r.Capability}
		prev, seen := merged[k]
		if !seen || rank(r.Result) > rank(prev.Result) {
			merged[k] = r
		}
	}
	out := make([]CompatRow, 0, len(merged))
	for _, r := range merged {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Harness != b.Harness {
			return a.Harness < b.Harness
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return a.Capability < b.Capability
	})
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "compat.json"), append(data, '\n'), 0o644)
}

// rank orders results: a failure outranks a pass, which outranks not having run.
func rank(result string) int {
	switch result {
	case "fail":
		return 2
	case "pass":
		return 1
	}
	return 0
}
