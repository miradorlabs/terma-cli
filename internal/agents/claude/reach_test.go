package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// reachMachine is a migrated machine where Acme Platform collects github.com/acme/*, bound to
// Acme, and personal only github.com/dawson/*: a personal checkout, side, with Acme's,
// acme-api, beside it. Every folder outside git, and github.com/other/*, is nobody's.
type reachMachine struct {
	t         *testing.T
	stateDir  string
	sp        *spool.Spool
	st        *routing.State
	side, api string
}

const reachSession = "reach-session"

func newReachMachine(t *testing.T) *reachMachine {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	side := hookruntest.RepoAt(t, "github.com/dawson/side")
	api := filepath.Join(filepath.Dir(side), "acme-api")
	if err := os.Rename(hookruntest.RepoAt(t, "github.com/acme/api"), api); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, side, "README.md", "side\n")
	hookruntest.WriteFile(t, api, "NOTES.md", "acme\n")
	st := hookruntest.RoutingState(t, []routing.Team{
		hookruntest.RoutedTeam("acme", "acme-platform", true, "github.com/acme/*"),
		hookruntest.RoutedPersonal("github.com/dawson/*"),
	}, map[string]string{"github.com/acme": "acme"})
	return &reachMachine{t: t, stateDir: t.TempDir(), sp: sp, st: st, side: side, api: api}
}

// run is one hook of the session in cwd, as dispatch runs it: the handler, then, when it
// claimed nothing, the session's placement from the payload.
func (m *reachMachine) run(handler func(context.Context, hookrun.Env) error, cwd string, payload map[string]any) {
	m.t.Helper()
	payload["session_id"], payload["cwd"] = reachSession, cwd
	b, err := json.Marshal(payload)
	if err != nil {
		m.t.Fatal(err)
	}
	claimed := false
	env := hookrun.Env{StateDir: m.stateDir, ConfigDir: m.t.TempDir(), Now: time.Now(), Cwd: cwd, Spool: m.sp,
		Policy: config.NoPolicy("", ""), Routing: hookrun.NewRouting(hookruntest.RoutedProfile, m.st),
		Stdin: bytes.NewReader(b), OnClaim: func(string) { claimed = true }}
	if err := handler(context.Background(), env); err != nil {
		m.t.Fatal(err)
	}
	if !claimed {
		hookrun.ClaimFromPayload(context.Background(), env, hookrun.PayloadSession{ID: reachSession, Cwd: cwd}, claudeTool)
	}
}

func (m *reachMachine) start(cwd string) {
	m.t.Helper()
	m.run(sessionStart, cwd, map[string]any{"hook_event_name": "SessionStart", "source": "startup"})
}

// tool is a call of tool in cwd with input, which reaches terma only where the PostToolUse
// matcher setup writes lets it, as Claude Code matches it: the whole tool name.
func (m *reachMachine) tool(cwd, tool string, input map[string]any) {
	m.t.Helper()
	i := slices.IndexFunc(claudeHooks, func(h struct{ Event, Matcher, Hook string }) bool { return h.Event == "PostToolUse" })
	if !regexp.MustCompile(`^(?:` + claudeHooks[i].Matcher + `)$`).MatchString(tool) {
		return
	}
	m.run(postToolUse, cwd, map[string]any{"hook_event_name": "PostToolUse", "tool_name": tool, "tool_input": input, "tool_use_id": "toolu_1"})
}

// mixed reports whether the session's run is Mixed, in the run ledger and its placement alike.
func (m *reachMachine) mixed() bool {
	m.t.Helper()
	_, ledger := runs.MixedAt(m.stateDir, reachSession, time.Time{})
	c, ok := claim.Read(m.stateDir, reachSession, time.Now())
	if placement := ok && c.Kind == claim.KindMixed; placement != ledger {
		m.t.Fatalf("ledger Mixed %v, mixed placement %v", ledger, placement)
	}
	return ledger
}

// spooled is what the spool took in since the last call.
func (m *reachMachine) spooled() []spool.Event { return hookruntest.Spooled(m.t, m.sp) }

// A personal session that reads or searches Acme's checkout has crossed: its run turns
// Mixed. Reading or searching its own checkout does not.
func TestRoutedClaudeReadsAndSearchesReachingAcmeMakeTheRunMixed(t *testing.T) {
	for _, tc := range []struct {
		tool  string
		input func(root string) map[string]any
	}{
		{"Read", func(root string) map[string]any { return map[string]any{"file_path": filepath.Join(root, "README.md")} }},
		{"Grep", func(root string) map[string]any { return map[string]any{"pattern": "TODO", "path": root} }},
		{"Glob", func(root string) map[string]any { return map[string]any{"pattern": "**/*.md", "path": root} }},
		{"LS", func(root string) map[string]any { return map[string]any{"path": root} }},
		{"NotebookRead", func(root string) map[string]any {
			return map[string]any{"notebook_path": filepath.Join(root, "a.ipynb")}
		}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			m := newReachMachine(t)
			m.start(m.side)
			m.tool(m.side, tc.tool, tc.input(m.side))
			if m.mixed() {
				t.Fatal("Mixed after reaching only its own checkout")
			}
			m.tool(m.side, tc.tool, tc.input(m.api))
			if !m.mixed() {
				t.Fatal("not Mixed after reaching Acme's checkout")
			}
		})
	}
}

// A shell command reaches the paths it names that are there, and every cd's target.
func TestRoutedClaudeShellCommandsReachingAcmeMakeTheRunMixed(t *testing.T) {
	for name, tc := range map[string]struct {
		command func(m *reachMachine) string
		mixed   bool
	}{
		"an append to Acme's file":          {func(m *reachMachine) string { return "echo x >> " + filepath.Join(m.api, "NOTES.md") }, true},
		"a cd into Acme's checkout":         {func(*reachMachine) string { return "cd ../acme-api && ls" }, true},
		"a listing of its own checkout":     {func(*reachMachine) string { return "ls -la" }, false},
		"a path that is not there, in Acme": {func(*reachMachine) string { return "cat ../acme-api/MISSING.md" }, false},
	} {
		t.Run(name, func(t *testing.T) {
			m := newReachMachine(t)
			m.start(m.side)
			m.tool(m.side, "Bash", map[string]any{"command": tc.command(m), "description": name})
			if got := m.mixed(); got != tc.mixed {
				t.Fatalf("Mixed %v, want %v", got, tc.mixed)
			}
		})
	}
}

// A read or a shell command only classifies: it spools nothing and records no manifest, in
// its own checkout or Acme's, though reaching Acme's makes the run Mixed. An edit records
// and spools as it always has.
func TestRoutedClaudeReadsAndShellCommandsOnlyClassify(t *testing.T) {
	m := newReachMachine(t)
	m.start(m.side)
	m.spooled() // the session's start
	m.tool(m.side, "Read", map[string]any{"file_path": filepath.Join(m.side, "README.md")})
	m.tool(m.side, "Bash", map[string]any{"command": "cat ./README.md"})
	m.tool(m.side, "Read", map[string]any{"file_path": filepath.Join(m.api, "NOTES.md")})
	m.tool(m.side, "Bash", map[string]any{"command": "cat ../acme-api/NOTES.md"})
	if !m.mixed() {
		t.Fatal("reading Acme's checkout did not make the run Mixed")
	}
	if evs := m.spooled(); len(evs) != 0 {
		t.Fatalf("reads spooled %s", hookruntest.Names(evs))
	}
	for _, root := range []string{m.side, m.api} {
		if manifests, _ := hookruntest.Store(t, root).Manifests(); len(manifests) != 0 {
			t.Fatalf("reads recorded manifests in %s: %+v", root, manifests)
		}
	}
	m.tool(m.side, "Edit", map[string]any{"file_path": filepath.Join(m.side, "README.md")})
	touches := hookruntest.Named(m.spooled(), semconv.TermaFilesTouchedEvent)
	if len(touches) != 1 || !slices.Equal(touches[0].Attrs[semconv.TermaFilesPathsKey].([]any), []any{"README.md"}) {
		t.Fatalf("an edit spooled %+v", touches)
	}
	if manifests, _ := hookruntest.Store(t, m.side).Manifests(); len(manifests) != 1 {
		t.Fatalf("an edit recorded manifests %+v", manifests)
	}
}

// A session whose own folder Route drops (outside git, or a repository nobody collects)
// sends nothing, but still records in the run ledger the Acme checkout it reached: placed
// in personal's checkout by a later hook, the run has crossed.
func TestRoutedClaudeSessionInADroppedFolderStillRecordsWhatItReached(t *testing.T) {
	for _, tc := range []struct {
		where string
		at    func(t *testing.T) string
		tool  string
	}{
		{"outside git", func(t *testing.T) string { d, _ := filepath.EvalSymlinks(t.TempDir()); return d }, "Edit"},
		{"outside git", func(t *testing.T) string { d, _ := filepath.EvalSymlinks(t.TempDir()); return d }, "Read"},
		{"a repository nobody collects", func(t *testing.T) string { return hookruntest.RepoAt(t, "github.com/other/lib") }, "Edit"},
	} {
		t.Run(tc.where+", "+tc.tool, func(t *testing.T) {
			m := newReachMachine(t)
			own := tc.at(t)
			m.start(own)
			m.tool(own, tc.tool, map[string]any{"file_path": filepath.Join(m.api, "NOTES.md")})
			if evs := m.spooled(); len(evs) != 0 {
				t.Fatalf("a dropped folder spooled %s", hookruntest.Names(evs))
			}
			if m.mixed() {
				t.Fatal("Mixed before the run was anywhere an organization collects")
			}
			if _, locs, _ := runs.Locations(m.stateDir, reachSession); !slices.Contains(locs, "github.com/acme/api") {
				t.Fatalf("the run ledger records %v, without Acme's checkout", locs)
			}
			m.tool(m.side, "Read", map[string]any{"file_path": filepath.Join(m.side, "README.md")})
			if !m.mixed() {
				t.Fatal("placed in personal's checkout after reaching Acme's, the run is not Mixed")
			}
		})
	}
}
