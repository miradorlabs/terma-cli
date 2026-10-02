package builtin

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// One name per adapter, one adapter per event, and no flush after an unhandled event.
func TestRegistryIsConsistent(t *testing.T) {
	names := map[string]bool{}
	owners := map[string]string{}
	for _, a := range reg.All() {
		if a.Name() == "" || a.Name() != strings.ToLower(a.Name()) {
			t.Errorf("adapter name %q must be a lowercase token", a.Name())
		}
		if names[a.Name()] {
			t.Errorf("adapter %q registered twice", a.Name())
		}
		names[a.Name()] = true
		if len(a.Events()) == 0 {
			t.Errorf("%s declares no events", a.Name())
		}
		for event, h := range a.Events() {
			if h == nil {
				t.Errorf("%s: event %q has no handler", a.Name(), event)
			}
			if owner, dup := owners[event]; dup {
				t.Errorf("event %q claimed by both %s and %s", event, owner, a.Name())
			}
			owners[event] = a.Name()
			if a.Name() != "claude" && !strings.HasPrefix(event, a.Name()+"-") {
				// Claude Code's committed events predate the prefix convention.
				t.Errorf("%s: event %q should be prefixed with the adapter name", a.Name(), event)
			}
		}
		for _, event := range a.FlushAfter() {
			if _, ok := a.Events()[event]; !ok {
				t.Errorf("%s flushes after %q, which it does not handle", a.Name(), event)
			}
		}
	}
	if len(reg.Handlers()) != len(owners) {
		t.Errorf("reg.Handlers() has %d entries, adapters declare %d", len(reg.Handlers()), len(owners))
	}
	for _, name := range []string{"claude", "cursor", "codex", "opencode", "omp", "antigravity"} {
		if _, ok := reg.Lookup(name); !ok {
			t.Errorf("reg.Lookup(%q) failed", name)
		}
	}
	if _, ok := reg.Lookup("zed"); ok {
		t.Error("Lookup accepted an unknown adapter")
	}
}

// Committed hook event names are pinned: renaming one breaks every repository that installed it.
func TestEventNamesAreStable(t *testing.T) {
	want := []string{
		"antigravity-post-invocation", "antigravity-post-tool-use", "antigravity-pre-invocation", "antigravity-stop",
		"codex-notify", "codex-permission-request", "codex-post-tool-use", "codex-pre-tool-use", "codex-session-end", "codex-session-start", "codex-stop",
		"codex-subagent-start", "codex-subagent-stop", "codex-user-prompt-submit",
		"cursor-after-agent-response", "cursor-before-submit-prompt", "cursor-file-edit", "cursor-post-tool-use",
		"cursor-post-tool-use-failure", "cursor-pre-compact", "cursor-session-end", "cursor-session-start",
		"cursor-stop", "cursor-subagent-stop",
		"dsh-file-edit", "dsh-prompt", "dsh-session-end", "dsh-session-start",
		"gemini-after-tool", "gemini-prompt", "gemini-session-end", "gemini-session-start",
		"hermes-file-edit", "hermes-prompt", "hermes-session-end", "hermes-session-start",
		"omp-file-edit", "omp-prompt", "omp-session-end", "omp-session-start",
		"opencode-file-edit", "opencode-session-end", "opencode-session-start",
		"pi-file-edit", "pi-prompt", "pi-session-end", "pi-session-start",
		"post-tool-use", "pre-tool-use", "session-end", "session-start", "stop", "stop-failure", "subagent-start", "subagent-stop",
		"user-prompt-submit",
	}
	got := slices.Sorted(maps.Keys(reg.Handlers()))
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event names changed:\n got %v\nwant %v", got, want)
	}
	for _, event := range []string{"stop", "cursor-stop", "codex-stop", "antigravity-stop", "session-end"} {
		if !reg.FlushesAfter(event) {
			t.Errorf("%s should flush the spool", event)
		}
	}
	if reg.FlushesAfter("post-tool-use") || reg.FlushesAfter("antigravity-post-tool-use") {
		t.Error("a per-tool-call hook must not start a flush")
	}
}

// Only Claude Code is wired unconditionally; every other adapter waits for its directory.
func TestDefaultsFollowTheRepositoryLayout(t *testing.T) {
	root := t.TempDir()
	defaults := func() []string {
		var out []string
		for _, a := range reg.All() {
			if a.Default(root) {
				out = append(out, a.Name())
			}
		}
		return out
	}
	if got := defaults(); strings.Join(got, ",") != "claude" {
		t.Fatalf("empty repository defaults = %v, want claude only", got)
	}
	for _, dir := range []string{".cursor", ".codex", ".agents", ".omp"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := defaults(); strings.Join(got, ",") != "claude,cursor,codex,omp,antigravity" {
		t.Fatalf("defaults = %v", got)
	}
	// Every repo-scope adapter plans a file on an empty repository; OpenCode plans none.
	for _, a := range reg.All() {
		p, err := a.Plan(t.TempDir(), true)
		if err != nil {
			t.Fatalf("%s: %v", a.Name(), err)
		}
		if (a.HooksPath() == "") != p.Empty() {
			t.Errorf("%s: HooksPath %q but plan empty=%v", a.Name(), a.HooksPath(), p.Empty())
		}
		for _, c := range p.Changes {
			if c.Path != a.HooksPath() {
				t.Errorf("%s plans %s, declares %s", a.Name(), c.Path, a.HooksPath())
			}
		}
	}
	if strings.Join(reg.RepoNames(), ",") != "claude,cursor,codex,omp,antigravity" {
		t.Fatalf("RepoNames = %v", reg.RepoNames())
	}
}

// Two adapters claiming one event would fail nowhere else: Handlers keeps the later one.
func TestEventNamesAreUnique(t *testing.T) {
	owner := map[string]string{}
	for _, a := range reg.All() {
		for event := range a.Events() {
			if other, taken := owner[event]; taken {
				t.Errorf("%q is claimed by both %s and %s", event, other, a.Name())
			}
			owner[event] = a.Name()
		}
	}
	if len(owner) != len(reg.Handlers()) {
		t.Fatalf("Handlers has %d events, the adapters declare %d", len(reg.Handlers()), len(owner))
	}
}

var reg = Agents()

// An exporting agent's harness answers to the agent's name.
func TestHarnessesAnswerToTheirAgentsName(t *testing.T) {
	for _, e := range reg.With[agents.Exporting]() {
		if got := e.Harness().Name(); got != e.Name() {
			t.Errorf("%s's harness is named %q", e.Name(), got)
		}
	}
	if got := reg.HarnessNames(); !slices.Equal(got, []string{"claude", "codex", "opencode", "omp"}) {
		t.Errorf("harnesses %q", got)
	}
}

func TestHarnessRejectsAnUnknownAgent(t *testing.T) {
	if _, err := reg.Harness("gemini"); err == nil {
		t.Fatal("gemini has no harness terma configures, and was resolved")
	}
	for _, name := range reg.HarnessNames() {
		if _, err := reg.Harness(name); err != nil {
			t.Errorf("Harness(%q): %v", name, err)
		}
	}
}

// Every harness answers a status query against an empty sandbox without error.
func TestEveryHarnessReportsStatusInASandbox(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CODEX_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("TERMA_CONFIG_DIR", filepath.Join(dir, "terma"))
	for _, h := range reg.Harnesses() {
		st, err := h.Status()
		if err != nil {
			t.Errorf("%s.Status: %v", h.Name(), err)
			continue
		}
		if st.Connected || st.Exists {
			t.Errorf("%s reported connected=%v exists=%v in an empty sandbox", h.Name(), st.Connected, st.Exists)
		}
	}
}

// Every harness is a full agent in the support catalog.
func TestSupportCatalogCoversEveryHarness(t *testing.T) {
	for _, h := range reg.Harnesses() {
		a, ok := reg.LookupSupport(h.Name())
		if !ok {
			t.Errorf("harness %q is not in the support catalog", h.Name())
			continue
		}
		if a.Telemetry.Level != agents.SupportFull || a.Support != agents.SupportFull {
			t.Errorf("%s exports telemetry but the catalog says telemetry %q, overall %q", h.Name(), a.Telemetry.Level, a.Support)
		}
	}
}
