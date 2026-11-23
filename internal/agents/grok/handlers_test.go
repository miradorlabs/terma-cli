package grok_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/dispatch"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// testdata/<fixture>.jsonl are the hooks grok 1.0.92 ran for terma's hooks file in each
// capture, in order, each the event and the payload on its stdin, unchanged but for the
// scrub (the space's fixtures-scrubbed): the events terma hooks, and PostToolUse for the
// tools its matcher names. The capture's repository is /tmp/grokcap/cases/<case>/repo, and a
// worktree subagent's checkout ~/.grok/worktrees/<repo>/subagent-<child>.
var (
	captureRepo     = regexp.MustCompile(`/tmp/grokcap/cases/[^/"]+/repo`)
	captureWorktree = regexp.MustCompile(`/home/dev/\.grok/worktrees/[^/"]+/subagent-[0-9a-f-]+`)
)

const (
	// The sessions of headless_subagent_worktree: grok ran its subagent in a linked worktree.
	worktreeParent = "01a20a05-7443-7162-b2d7-43ca11701b1a"
	worktreeChild  = "01a20a05-a8e1-7b13-8d64-c57716a14568"
	// The sessions of headless_edit_background_subagent: grok ran its subagent in the background.
	backgroundParent = "01a20a07-3e68-74e1-91d1-18ba6bd8f10a"
)

type capturedHook struct {
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload"`
}

// hookInfo is what a test reads of a captured payload, in the keys grok sends.
type hookInfo struct {
	SessionID    string          `json:"session_id"`
	PromptID     string          `json:"promptId"`
	SubagentID   string          `json:"subagentId"`
	SubagentType string          `json:"subagentType"`
	ToolName     string          `json:"tool_name"`
	ToolUseID    string          `json:"tool_use_id"`
	ToolResponse json.RawMessage `json:"tool_response"`
}

func (h capturedHook) info(t *testing.T) hookInfo {
	t.Helper()
	var in hookInfo
	if err := json.Unmarshal(h.Payload, &in); err != nil {
		t.Fatal(err)
	}
	return in
}

func readCaptured(t *testing.T, fixture string) []capturedHook {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", fixture+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []capturedHook
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var h capturedHook
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

// replayRun is what replaying a capture through dispatch left.
type replayRun struct {
	root, worktree, stateDir string
	sp                       *spool.Spool
	hooks                    []capturedHook
	stdout                   []string
	// active is the session each store held as active after each hook, by hook.
	active [][]string
}

// replayOpts change a replay: the agents the developer chose, the policy, and a payload's
// edit before it runs.
type replayOpts struct {
	agents []string
	policy func(config.Policy) config.Policy
	edit   func(event string, payload []byte) []byte
}

// replay runs every captured hook of fixture through dispatch.Run as grok runs terma's
// hooks file, in a repository the team collects, with a linked worktree for a worktree
// subagent.
func replay(t *testing.T, fixture string, o replayOpts) *replayRun {
	t.Helper()
	root, stateDir, sp := hookruntest.Project(t)
	hookruntest.RelayOn(t, stateDir)
	run := &replayRun{root: root, stateDir: stateDir, sp: sp, hooks: readCaptured(t, fixture)}
	if o.agents == nil {
		o.agents = []string{"grok"}
	}
	stores := []*session.Store{hookruntest.Store(t, root)}
	if raw, _ := json.Marshal(run.hooks); captureWorktree.Match(raw) {
		hookruntest.WriteFile(t, root, "a.txt", "hello\n")
		for _, args := range [][]string{{"add", "a.txt"}, {"commit", "-q", "-m", "init"}} {
			if _, err := gitx.Git(context.Background(), root, args...); err != nil {
				t.Fatal(err)
			}
		}
		run.worktree = filepath.Join(t.TempDir(), "subagent")
		if _, err := gitx.Git(context.Background(), root, "worktree", "add", "-q", "-b", "subagent", run.worktree); err != nil {
			t.Fatal(err)
		}
		run.worktree, _ = filepath.EvalSymlinks(run.worktree)
		wtRoot, gitDir, ok := gitx.LocateFS(run.worktree)
		if !ok {
			t.Fatal("the worktree is no checkout")
		}
		dir, err := project.StoreDir(stateDir, wtRoot, gitDir)
		if err != nil {
			t.Fatal(err)
		}
		stores = append(stores, session.Open(dir))
	}
	deps := dispatch.Deps{
		ConfigDir: t.TempDir(),
		StateDir:  stateDir,
		Agents:    reg,
		Profile: func() dispatch.Profile {
			p := hookruntest.Admitting(root)
			p.TeamID, p.FetchedAt = hookruntest.Team, time.Now()
			if o.policy != nil {
				p = o.policy(p)
			}
			return dispatch.Profile{Team: hookruntest.Team, Agents: o.agents, Account: hookruntest.Account, Policy: p}
		},
		Spool:   func() *spool.Spool { return sp },
		Claimed: func(string, config.Policy) {},
		Flush:   func() {},
	}
	for _, h := range run.hooks {
		payload := captureRepo.ReplaceAllString(string(h.Payload), hookruntest.InJSON(root))
		if run.worktree != "" {
			payload = captureWorktree.ReplaceAllString(payload, hookruntest.InJSON(run.worktree))
		}
		if o.edit != nil {
			payload = string(o.edit(h.Event, []byte(payload)))
		}
		event := "grok-" + strings.ReplaceAll(h.Event, "_", "-")
		env := map[string]string{"GROK_HOOK_EVENT": h.Event, "CLAUDE_PROJECT_DIR": root}
		var stdout bytes.Buffer
		dispatch.Run(context.Background(), deps, dispatch.Request{
			Event: event, Stdin: strings.NewReader(payload), Stdout: &stdout, Stderr: io.Discard,
			Version: "test", Cwd: root, Getenv: func(k string) string { return env[k] },
		})
		if stdout.Len() > 0 {
			run.stdout = append(run.stdout, event+": "+stdout.String())
		}
		var active []string
		for _, s := range stores {
			if sess, ok := s.Active(time.Now(), 24*time.Hour); ok {
				active = append(active, sess.ID)
			}
		}
		run.active = append(run.active, active)
	}
	return run
}

// sessions are the sessions the captured hooks ran in, and the children SubagentStart named.
func (r *replayRun) sessions(t *testing.T) (all, children []string) {
	t.Helper()
	for _, h := range r.hooks {
		in := h.info(t)
		for _, id := range []string{in.SessionID, in.SubagentID} {
			if id != "" && !slices.Contains(all, id) {
				all = append(all, id)
			}
		}
		if in.SubagentID != "" && !slices.Contains(children, in.SubagentID) {
			children = append(children, in.SubagentID)
		}
	}
	return all, children
}

var fixtures = []string{"headless_subagent_worktree", "tui_multi_turn_compact_new_clear", "headless_edit_background_subagent", "headless_fork_2_fork_session"}

// Every session grok ran is claimed for the team, once, as grok's; a subagent's claim names
// the session that opened it, which SubagentStart named, and no other claim names a parent.
// No subagent is ever the active session, and no hook says a word to grok.
func TestCapturedHooksClaimEverySession(t *testing.T) {
	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			run := replay(t, fixture, replayOpts{})
			all, children := run.sessions(t)
			parents := map[string]string{}
			for _, h := range run.hooks {
				if in := h.info(t); h.Event == "subagent_start" {
					parents[in.SubagentID] = in.SessionID
				}
			}
			for _, id := range all {
				c, ok := claim.Read(run.stateDir, id, time.Now())
				if !ok || c.ProjectID != hookruntest.Team || c.Tool != "grok" {
					t.Errorf("session %s: claim %+v, %v", id, c, ok)
				}
				if c.Parent != parents[id] {
					t.Errorf("session %s's claim names parent %q, want %q", id, c.Parent, parents[id])
				}
			}
			for i, active := range run.active {
				for _, child := range children {
					if slices.Contains(active, child) {
						t.Errorf("after hook %d (%s) the subagent %s is the active session", i, run.hooks[i].Event, child)
					}
				}
			}
			if len(run.stdout) != 0 {
				t.Errorf("replied %q", run.stdout)
			}
			if fixture == "headless_subagent_worktree" && len(children) != 1 {
				t.Errorf("subagents %v", children)
			}
		})
	}
}

// A subagent's start is spooled under its own session, naming its parent and its kind; a
// top-level session's names neither.
func TestCapturedSubagentStartNamesItsParent(t *testing.T) {
	run := replay(t, "headless_subagent_worktree", replayOpts{})
	starts := map[string]spool.Event{}
	for _, e := range hookruntest.Named(hookruntest.Spooled(t, run.sp), semconv.TermaSessionStartEvent) {
		starts[e.SessionID] = e
	}
	child, parent := starts[worktreeChild], starts[worktreeParent]
	if child.Attrs[semconv.TermaSessionParentIDKey] != worktreeParent || child.Attrs[semconv.GenAIAgentNameKey] != "general-purpose" {
		t.Errorf("the subagent's start: %v", child.Attrs)
	}
	if _, named := parent.Attrs[semconv.TermaSessionParentIDKey]; named || parent.SessionID == "" {
		t.Errorf("the parent's start: %+v", parent)
	}
}

// Each UserPromptSubmit is one terma.user.message, under its session, with the hook's
// promptId and, consented and collected, its text; a subagent's names its kind.
func TestCapturedPromptsAreOneMessageEach(t *testing.T) {
	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			run := replay(t, fixture, replayOpts{})
			messages := hookruntest.Named(hookruntest.Spooled(t, run.sp), semconv.TermaUserMessageEvent)
			var prompts []hookInfo
			for _, h := range run.hooks {
				if h.Event == "user_prompt_submit" {
					prompts = append(prompts, h.info(t))
				}
			}
			if len(messages) != len(prompts) || len(prompts) == 0 {
				t.Fatalf("%d messages for %d prompts", len(messages), len(prompts))
			}
			for i, m := range messages {
				p := prompts[i]
				if m.SessionID != p.SessionID || m.Attrs[semconv.TermaMessageIDKey] != p.PromptID || m.Attrs[semconv.GenAIMainAgentNameKey] != "grok" {
					t.Errorf("message %d: %s %v, want session %s, id %s", i, m.SessionID, m.Attrs, p.SessionID, p.PromptID)
				}
				if text, _ := m.Attrs[semconv.TermaMessageTextKey].(string); text == "" || m.Attrs[semconv.TermaMessageTruncatedKey] != false {
					t.Errorf("message %d carries text %q, truncated %v", i, text, m.Attrs[semconv.TermaMessageTruncatedKey])
				}
				if got, _ := m.Attrs[semconv.GenAIAgentNameKey].(string); got != p.SubagentType {
					t.Errorf("message %d names agent %q, want %q", i, got, p.SubagentType)
				}
				if _, turn := m.Attrs[semconv.TermaTurnIDKey]; turn {
					t.Errorf("message %d names a turn", i)
				}
			}
		})
	}
}

// A prompt's text travels only while grok is among the developer's agents and the policy
// collects prompts: otherwise the message still counts, without it. Text past the bound is
// cut on a character's edge, and says so.
func TestPromptTextFollowsConsentAndPolicy(t *testing.T) {
	for name, o := range map[string]replayOpts{
		"grok not chosen": {agents: []string{"claude"}},
		"prompts withheld": {policy: func(p config.Policy) config.Policy {
			p.IncludePrompts = false
			return p
		}},
	} {
		t.Run(name, func(t *testing.T) {
			run := replay(t, "headless_fork_2_fork_session", o)
			messages := hookruntest.Named(hookruntest.Spooled(t, run.sp), semconv.TermaUserMessageEvent)
			if len(messages) != 1 {
				t.Fatalf("%d messages", len(messages))
			}
			for _, key := range []string{semconv.TermaMessageTextKey, semconv.TermaMessageTruncatedKey} {
				if v, ok := messages[0].Attrs[key]; ok {
					t.Errorf("%s = %v", key, v)
				}
			}
		})
	}
	t.Run("cut", func(t *testing.T) {
		long := strings.Repeat("é", 10_000) // 20,000 bytes
		run := replay(t, "headless_fork_2_fork_session", replayOpts{edit: func(event string, payload []byte) []byte {
			if event != "user_prompt_submit" {
				return payload
			}
			var m map[string]any
			_ = json.Unmarshal(payload, &m)
			m["prompt"] = long
			out, _ := json.Marshal(m)
			return out
		}})
		m := hookruntest.Named(hookruntest.Spooled(t, run.sp), semconv.TermaUserMessageEvent)[0]
		text, _ := m.Attrs[semconv.TermaMessageTextKey].(string)
		if len(text) != 16<<10 || !strings.HasPrefix(long, text) || m.Attrs[semconv.TermaMessageTruncatedKey] != true {
			t.Errorf("text of %d bytes, truncated %v", len(text), m.Attrs[semconv.TermaMessageTruncatedKey])
		}
	})
}

// grok fires UserPromptSubmit for a turn it starts itself too: a background subagent that
// finishes while its parent is idle hands the parent a system reminder for a prompt, under an
// id of grok's own making (seen live on 1.0.92; the payload is otherwise a prompt hook's).
// Nobody wrote it, and grok's export sends no user_prompt for it: no message, and the session
// is still claimed.
func TestAPromptGrokWroteItselfIsNoMessage(t *testing.T) {
	run := replay(t, "headless_fork_2_fork_session", replayOpts{edit: func(event string, payload []byte) []byte {
		if event != "user_prompt_submit" {
			return payload
		}
		var m map[string]any
		_ = json.Unmarshal(payload, &m)
		m["promptId"] = "subagent-completed-01a20a78-86a5-72d2-8cb6-2c1d7201fb27"
		m["prompt"] = "<system-reminder>\nWhile you were idle, 1 background subagent completed.\n</system-reminder>"
		out, _ := json.Marshal(m)
		return out
	}})
	spooled := hookruntest.Spooled(t, run.sp)
	if n := len(hookruntest.Named(spooled, semconv.TermaUserMessageEvent)); n != 0 {
		t.Errorf("%d messages for a prompt grok wrote itself", n)
	}
	if len(hookruntest.Named(spooled, semconv.TermaSessionStartEvent)) == 0 {
		t.Error("the session was not announced")
	}
}

// A subagent's SessionEnd ends the subagent alone: its parent stays the active session.
func TestCapturedSubagentEndLeavesItsParentActive(t *testing.T) {
	for _, fixture := range []string{"headless_subagent_worktree", "headless_edit_background_subagent"} {
		t.Run(fixture, func(t *testing.T) {
			run := replay(t, fixture, replayOpts{})
			parent := map[string]string{"headless_subagent_worktree": worktreeParent, "headless_edit_background_subagent": backgroundParent}[fixture]
			seen := false
			for i, h := range run.hooks {
				if in := h.info(t); h.Event == "session_end" && in.SubagentType != "" {
					seen = true
					if !slices.Contains(run.active[i], parent) {
						t.Errorf("after the subagent's end, active sessions %v, want %s", run.active[i], parent)
					}
				}
			}
			if !seen {
				t.Fatal("the capture has no subagent's SessionEnd")
			}
		})
	}
}

// An applied search_replace is one touch of its file, with the call id the export's
// tool_use_id joins; a subagent's names the subagent, and stays in its own manifest.
func TestCapturedEditsAreTouchedWithTheirCallID(t *testing.T) {
	for _, fixture := range []string{"headless_subagent_worktree", "headless_edit_background_subagent"} {
		t.Run(fixture, func(t *testing.T) {
			run := replay(t, fixture, replayOpts{})
			var edits []hookInfo
			for _, h := range run.hooks {
				if in := h.info(t); h.Event == "post_tool_use" && strings.Contains(string(in.ToolResponse), `"EditsApplied"`) {
					edits = append(edits, in)
				}
			}
			touches := hookruntest.Named(hookruntest.Spooled(t, run.sp), semconv.TermaFilesTouchedEvent)
			if len(edits) != 1 || len(touches) != 1 {
				t.Fatalf("%d edits, %d touches", len(edits), len(touches))
			}
			e, touch := edits[0], touches[0]
			if touch.SessionID != e.SessionID || touch.Attrs[semconv.GenAIToolCallIDKey] != e.ToolUseID ||
				hookruntest.Joined(touch.Attrs[semconv.TermaFilesPathsKey]) != "a.txt" || touch.Attrs[semconv.TermaCheckoutIDKey] == nil {
				t.Errorf("touch %s %v, for %+v", touch.SessionID, touch.Attrs, e)
			}
			if child := e.SubagentType != ""; child && (touch.Attrs[semconv.TermaAgentIDKey] != e.SessionID || touch.Attrs[semconv.GenAIAgentNameKey] != e.SubagentType) {
				t.Errorf("a subagent's touch names agent %v", touch.Attrs)
			}
		})
	}
}

// A result that does not prove the edit sends no touch: another type, the write tool's (not
// yet known), or an edit that applied nothing.
func TestOnlyAnAppliedEditIsTouched(t *testing.T) {
	for name, result := range map[string]string{
		"read":     `{"type":"ReadFile","FileContent":{"content":"x"}}`,
		"write":    `{"type":"Write","path":"a.txt"}`,
		"not run":  `{"type":"SearchReplace","Error":"no match"}`,
		"not JSON": `"done"`,
	} {
		t.Run(name, func(t *testing.T) {
			run := replay(t, "headless_edit_background_subagent", replayOpts{edit: func(event string, payload []byte) []byte {
				if event != "post_tool_use" {
					return payload
				}
				var m map[string]json.RawMessage
				_ = json.Unmarshal(payload, &m)
				m["tool_response"], m["toolResult"] = json.RawMessage(result), json.RawMessage(result)
				out, _ := json.Marshal(m)
				return out
			}})
			if touches := hookruntest.Named(hookruntest.Spooled(t, run.sp), semconv.TermaFilesTouchedEvent); len(touches) != 0 {
				t.Errorf("touched %v", touches[0].Attrs)
			}
		})
	}
}
