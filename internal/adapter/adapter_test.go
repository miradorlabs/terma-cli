package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The registry is the single source every command reads. These are the invariants
// that make that safe: one name per adapter, one adapter per event, and no flush after
// an event nobody handles.
func TestRegistryIsConsistent(t *testing.T) {
	names := map[string]bool{}
	owners := map[string]string{}
	for _, a := range All() {
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
				// Claude Code's events predate the prefix convention and are committed
				// wiring; every later adapter namespaces its own.
				t.Errorf("%s: event %q should be prefixed with the adapter name", a.Name(), event)
			}
		}
		for _, event := range a.FlushAfter() {
			if _, ok := a.Events()[event]; !ok {
				t.Errorf("%s flushes after %q, which it does not handle", a.Name(), event)
			}
		}
	}
	if len(Handlers()) != len(owners) {
		t.Errorf("Handlers() has %d entries, adapters declare %d", len(Handlers()), len(owners))
	}
	for _, name := range []string{"claude", "cursor", "codex", "opencode", "antigravity"} {
		if _, ok := Lookup(name); !ok {
			t.Errorf("Lookup(%q) failed", name)
		}
	}
	if _, ok := Lookup("zed"); ok {
		t.Error("Lookup accepted an unknown adapter")
	}
}

// Committed hook event names are wiring other repositories depend on: renaming one
// breaks every repository that installed the old name. This pins the set.
func TestEventNamesAreStable(t *testing.T) {
	want := []string{
		"antigravity-post-invocation", "antigravity-post-tool-use", "antigravity-pre-invocation", "antigravity-stop",
		"codex-notify", "codex-permission-request", "codex-post-tool-use", "codex-pre-tool-use", "codex-session-end", "codex-session-start", "codex-stop",
		"codex-subagent-start", "codex-subagent-stop", "codex-user-prompt-submit",
		"cursor-after-agent-response", "cursor-before-submit-prompt", "cursor-file-edit", "cursor-post-tool-use",
		"cursor-post-tool-use-failure", "cursor-pre-compact", "cursor-session-end", "cursor-session-start",
		"cursor-stop", "cursor-subagent-stop",
		"opencode-file-edit", "opencode-session-end", "opencode-session-start",
		"post-tool-use", "session-end", "session-start", "stop", "stop-failure", "subagent-start", "subagent-stop",
	}
	got := EventNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("event names changed:\n got %v\nwant %v", got, want)
	}
	for _, event := range []string{"stop", "cursor-stop", "codex-stop", "antigravity-stop", "session-end"} {
		if !FlushesAfter(event) {
			t.Errorf("%s should flush the spool", event)
		}
	}
	if FlushesAfter("post-tool-use") || FlushesAfter("antigravity-post-tool-use") {
		t.Error("a per-tool-call hook must not start a flush")
	}
}

// Only Claude Code is wired into a repository unconditionally; every other adapter
// waits for evidence the repository is used with its agent.
func TestDefaultsFollowTheRepositoryLayout(t *testing.T) {
	root := t.TempDir()
	defaults := func() []string {
		var out []string
		for _, a := range All() {
			if a.Default(root) {
				out = append(out, a.Name())
			}
		}
		return out
	}
	if got := defaults(); strings.Join(got, ",") != "claude" {
		t.Fatalf("empty repository defaults = %v, want claude only", got)
	}
	for _, dir := range []string{".cursor", ".codex", ".agents"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := defaults(); strings.Join(got, ",") != "claude,cursor,codex,antigravity" {
		t.Fatalf("defaults = %v", got)
	}
	// Every repo-scope adapter plans a file on an empty repository; OpenCode plans none.
	for _, a := range All() {
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
	if strings.Join(RepoNames(), ",") != "claude,cursor,codex,antigravity" {
		t.Fatalf("RepoNames = %v", RepoNames())
	}
}

// Handlers folds every adapter's events into one map, so two adapters claiming one name
// would not fail anywhere: the later one would simply take the other agent's payloads.
func TestEventNamesAreUnique(t *testing.T) {
	owner := map[string]string{}
	for _, a := range All() {
		for event := range a.Events() {
			if other, taken := owner[event]; taken {
				t.Errorf("%q is claimed by both %s and %s", event, other, a.Name())
			}
			owner[event] = a.Name()
		}
	}
	if len(owner) != len(Handlers()) {
		t.Fatalf("Handlers has %d events, the adapters declare %d", len(Handlers()), len(owner))
	}
}
