package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/dispatch"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// grokHook is one hook process grok 1.0.92 started, as captured: its environment and the
// payload on its stdin, the workspace scrubbed to /work/repo.
type grokHook struct {
	Event   string          `json:"env_event"`
	Name    string          `json:"env_name"`
	Session string          `json:"env_session"`
	Payload json.RawMessage `json:"payload"`
}

// grokPayloadFor is the grok event whose payload each Claude Code and Cursor event runs
// with: its own, or the nearest.
var grokPayloadFor = map[string]string{
	"session-start":      "session_start",
	"user-prompt-submit": "user_prompt_submit",
	"post-tool-use":      "post_tool_use",
	"subagent-start":     "subagent_start",
	"subagent-stop":      "subagent_stop",
	"stop":               "stop",
	"stop-failure":       "stop",
	"session-end":        "session_end",

	"cursor-session-start":         "session_start",
	"cursor-before-submit-prompt":  "user_prompt_submit",
	"cursor-post-tool-use":         "post_tool_use",
	"cursor-post-tool-use-failure": "post_tool_use",
	"cursor-file-edit":             "post_tool_use",
	"cursor-after-agent-response":  "stop",
	"cursor-stop":                  "stop",
	"cursor-pre-compact":           "stop",
	"cursor-subagent-stop":         "subagent_stop",
	"cursor-session-end":           "session_end",
}

// grokHooks reads the captured hooks, in the order grok started them.
func grokHooks(t *testing.T) []grokHook {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "grok_hooks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []grokHook
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var h grokHook
		if err := json.Unmarshal(sc.Bytes(), &h); err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// grokRun is what running every Claude Code and Cursor event with grok's payloads left.
type grokRun struct {
	root, stateDir   string
	sp               *spool.Spool
	claims, flushes  int
	stdout, sessions []string
}

// runGrokHooks runs, for each captured hook in order, every Claude Code and Cursor event
// mapped to it through dispatch.Run, with grok's environment when grok is set.
func runGrokHooks(t *testing.T, grok bool) *grokRun {
	t.Helper()
	root, stateDir, sp := hookruntest.Project(t)
	hookruntest.RelayOn(t, stateDir)
	run := &grokRun{root: root, stateDir: stateDir, sp: sp}
	deps := dispatch.Deps{
		ConfigDir: t.TempDir(),
		StateDir:  stateDir,
		Agents:    reg,
		Profile: func() dispatch.Profile {
			p := hookruntest.Admitting(root)
			p.TeamID, p.FetchedAt = hookruntest.Team, time.Now()
			return dispatch.Profile{Team: hookruntest.Team, Account: hookruntest.Account, Policy: p}
		},
		Spool:   func() *spool.Spool { return sp },
		Claimed: func(string, config.Policy) { run.claims++ },
		Flush:   func() { run.flushes++ },
	}
	var events []string
	for _, owner := range []string{"claude", "cursor"} {
		a, ok := reg.Lookup(owner)
		if !ok {
			t.Fatalf("no %s agent", owner)
		}
		events = append(events, slices.Sorted(maps.Keys(a.Events()))...)
	}
	for _, event := range events {
		if _, ok := grokPayloadFor[event]; !ok {
			t.Fatalf("%s has no grok payload mapped", event)
		}
	}
	for _, h := range grokHooks(t) {
		if !slices.Contains(run.sessions, h.Session) {
			run.sessions = append(run.sessions, h.Session)
		}
		env := map[string]string{"CLAUDE_PROJECT_DIR": root}
		if grok {
			env["GROK_HOOK_EVENT"], env["GROK_HOOK_NAME"], env["GROK_SESSION_ID"], env["GROK_WORKSPACE_ROOT"] = h.Event, h.Name, h.Session, root
		}
		payload := strings.ReplaceAll(string(h.Payload), "/work/repo", hookruntest.InJSON(root))
		for _, event := range events {
			if grokPayloadFor[event] != h.Event {
				continue
			}
			var stdout, stderr bytes.Buffer
			dispatch.Run(context.Background(), deps, dispatch.Request{
				Event: event, Stdin: strings.NewReader(payload), Stdout: &stdout, Stderr: &stderr,
				Version: "test", Cwd: root, Getenv: func(k string) string { return env[k] },
			})
			if stdout.Len() > 0 {
				run.stdout = append(run.stdout, event+": "+stdout.String())
			}
		}
	}
	return run
}

// grok runs the entries in Claude Code's and Cursor's hooks files. A hook process grok
// started records nothing, claims nothing and replies nothing, whichever event it runs;
// the same payloads without grok's environment record a Claude Code session.
func TestGrokHooksStandDown(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())

	t.Run("grok", func(t *testing.T) {
		run := runGrokHooks(t, true)
		if events := hookruntest.Spooled(t, run.sp); len(events) != 0 {
			t.Errorf("spooled %s", hookruntest.Names(events))
		}
		for _, id := range run.sessions {
			if c, ok := claim.Read(run.stateDir, id, time.Now()); ok {
				t.Errorf("session %s claimed or marked: %+v", id, c)
			}
		}
		if run.claims != 0 || run.flushes != 0 {
			t.Errorf("%d claims, %d flushes", run.claims, run.flushes)
		}
		if len(run.stdout) != 0 {
			t.Errorf("replied %q", run.stdout)
		}
		if _, err := os.Stat(filepath.Join(run.root, ".git", project.GitStoreDir)); !os.IsNotExist(err) {
			t.Errorf("the repository has a session store: %v", err)
		}
		token := claim.TokenPath(run.stateDir)
		_ = filepath.WalkDir(run.stateDir, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && path != token {
				t.Errorf("state written: %s", path)
			}
			return nil
		})
	})

	// Control: the same hooks, without grok's environment, record and claim the session.
	t.Run("without grok", func(t *testing.T) {
		run := runGrokHooks(t, false)
		events := hookruntest.Spooled(t, run.sp)
		main := run.sessions[0]
		claudeCode, _ := reg.Lookup("claude")
		for _, name := range []string{semconv.TermaSessionStartEvent, semconv.TermaSessionEndEvent} {
			found := false
			for _, e := range hookruntest.Named(events, name) {
				if e.SessionID == main && e.Attrs[semconv.GenAIMainAgentNameKey] == agents.Tool(claudeCode) {
					found = true
				}
			}
			if !found {
				t.Errorf("no %s for %s as Claude Code in %s", name, main, hookruntest.Names(events))
			}
		}
		if c, ok := claim.Read(run.stateDir, main, time.Now()); !ok || c.ProjectID == "" {
			t.Errorf("claim %+v, %v", c, ok)
		}
		if run.claims == 0 {
			t.Error("Claimed never ran")
		}
	})
}
