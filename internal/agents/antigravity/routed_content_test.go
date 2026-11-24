package antigravity

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/dispatch"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

const routedAccount = "1111111111111111"

// routedTeam is a routing state of one organization team that lists and is bound to the
// repository InitRepo made at root, with its policy's content switches as given.
func routedTeam(t *testing.T, root, org, team string, prompts, tools bool) routing.State {
	t.Helper()
	repo := "github.com/acme/" + filepath.Base(root)
	st, err := routing.Create(t.TempDir(), "default", routedAccount, func(s *routing.State) error {
		if _, err := s.Select(routing.Team{OrganizationID: org, AuthURL: "https://api.example.com", TeamID: team, Policy: config.Policy{
			Mode: config.ModeRepo, Repositories: []string{repo}, IncludePrompts: prompts, IncludeToolContent: tools,
			Revision: 1, FetchedAt: time.Now().UTC(), TeamID: team, OrganizationID: org, AuthURL: "https://api.example.com"}}); err != nil {
			return err
		}
		return s.Bind(repo, org, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// Through dispatch.Run, as `terma hook` runs it on a routed machine, a hook's content follows
// the policy of the team routing sends its checkout to: dispatch hands every handler no
// policy of its own, which once withheld every prompt, reply, argument and result.
func TestAntigravityContentFollowsTheRoutedTeamsPolicyThroughDispatch(t *testing.T) {
	for _, tc := range []struct {
		name           string
		prompts, tools bool
	}{
		{name: "content on", prompts: true, tools: true},
		{name: "prompts only", prompts: true},
		{name: "tool content only", tools: true},
		{name: "content off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
			hookruntest.RelayOn(t, g.stateDir)
			st := routedTeam(t, g.root, "orgA", "team-a", tc.prompts, tc.tools)
			enrollment := st.Teams[0].EnrollmentID
			deps := dispatch.Deps{
				StateDir: g.stateDir, Agents: agents.New(Agent{}),
				Profile: func() dispatch.Profile {
					return dispatch.Profile{Agents: g.agents, Routing: hookrun.NewRouting("default", &st), Account: routedAccount}
				},
				Spool: func() *spool.Spool { return g.sp },
				Flush: func() {},
			}
			hook := func(event, extra string) {
				t.Helper()
				var out bytes.Buffer
				if status := dispatch.Run(context.Background(), deps, dispatch.Request{Event: event, Cwd: g.root,
					Stdin: strings.NewReader(g.payload(g.root, extra)), Stdout: &out, Stderr: &out, Version: "test"}); status != 0 {
					t.Fatalf("%s: status %d: %s", event, status, out.String())
				}
			}
			// The fixture's first turn, as turnOne runs it.
			g.write(1)
			hook("antigravity-pre-invocation", begin("1"))
			g.write(2)
			hook("antigravity-post-tool-use", toolUse(2, "write_file", `{"Path":"`+hookruntest.InJSON(filepath.Join(g.root, "a.txt"))+`","Content":"made-up content","Overwrite":true}`))
			g.write(4)
			hook("antigravity-post-tool-use", toolUse(4, "run_command", `{"CommandLine":"made-up command","WaitMsBeforeAsync":5000}`))
			g.write(7)
			hook("antigravity-post-invocation", invoked)
			hook("antigravity-stop", stopped)
			got := g.drain()

			calls := got[semconv.TermaToolCallEvent]
			if len(calls) != 2 {
				t.Fatalf("%d tool calls, want 2: %v", len(calls), calls)
			}
			for _, c := range calls {
				if c.Enrollment != enrollment || c.Profile != "default" || c.Account != routedAccount {
					t.Errorf("tool call routed to %q/%q as %q, want the team's enrollment %q", c.Profile, c.Enrollment, c.Account, enrollment)
				}
				if _, args := c.Attrs[semconv.GenAIToolCallArgumentsKey]; args != tc.tools {
					t.Errorf("arguments on %v: %v, want %v", c.Attrs[semconv.GenAIToolCallIDKey], args, tc.tools)
				}
			}
			if n := len(got[semconv.TermaToolResultEvent]); (n == 2) != tc.tools || n != 0 && !tc.tools {
				t.Errorf("%d tool results with tool content %v: %v", n, tc.tools, got[semconv.TermaToolResultEvent])
			}
			prompt, reply := false, false
			for _, e := range got[semconv.TermaUserMessageEvent] {
				prompt = prompt || e.Attrs[semconv.TermaMessageTextKey] == "made-up prompt 0"
			}
			for _, e := range got[semconv.TermaAssistantMessageEvent] {
				reply = reply || e.Attrs[semconv.TermaMessageTextKey] == "made-up reply 6"
			}
			if prompt != tc.prompts || reply != tc.prompts {
				t.Errorf("prompt sent %v, reply sent %v, want both %v", prompt, reply, tc.prompts)
			}
			if !tc.prompts && !tc.tools && (g.spoke(0) || g.spoke(2) || g.spoke(4) || g.spoke(6)) {
				t.Errorf("content left with the team's content switched off")
			}
		})
	}
}

// A workspace is collected with the hook's checkout only where routing sends both to the
// same team: another team's repository, or one the developer bound elsewhere, shares none of
// the conversation's content, whatever the hook's own team lists.
func TestAntigravityRoutedWorkspacesShareOnlyTheSameTeam(t *testing.T) {
	g := newTranscriptRig(t, turnsConversation, "transcript_turns.jsonl")
	other := hookruntest.InitRepo(t)
	now := time.Now().UTC()
	pol := func(org, team string, repos ...string) config.Policy {
		return config.Policy{Mode: config.ModeRepo, Repositories: repos, IncludePrompts: true, IncludeToolContent: true,
			Revision: 1, FetchedAt: now, TeamID: team, OrganizationID: org, AuthURL: "https://api.example.com"}
	}
	mine, theirs := "github.com/acme/"+filepath.Base(g.root), "github.com/acme/"+filepath.Base(other)
	st, err := routing.Create(t.TempDir(), "default", routedAccount, func(s *routing.State) error {
		// Team A lists both repositories; the developer bound the other one to organization B.
		for _, team := range []routing.Team{
			{OrganizationID: "orgA", AuthURL: "https://api.example.com", TeamID: "team-a", Policy: pol("orgA", "team-a", mine, theirs)},
			{OrganizationID: "orgB", AuthURL: "https://api.example.com", TeamID: "team-b", Policy: pol("orgB", "team-b", theirs)},
		} {
			if _, err := s.Select(team); err != nil {
				return err
			}
		}
		if err := s.Bind(mine, "orgA", false); err != nil {
			return err
		}
		return s.Bind(theirs, "orgB", false)
	})
	if err != nil {
		t.Fatal(err)
	}
	e := hookrun.Env{StateDir: g.stateDir, Now: now, Cwd: g.root, Agents: g.agents, Policy: config.NoPolicy("", ""), Routing: hookrun.NewRouting("default", &st), Account: routedAccount}
	r, err := e.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if prompts, tools := e.PolicyFor(r).Content(); !prompts || !tools {
		t.Fatalf("the routed team's policy: prompts %v, tool content %v", prompts, tools)
	}
	paths := newAntigravityPaths(e, r)
	if !paths.workspacesCollected(g.root, &antigravityHookInput{WorkspacePaths: []string{g.root}}) {
		t.Error("the hook's own checkout is not collected")
	}
	if paths.workspacesCollected(g.root, &antigravityHookInput{WorkspacePaths: []string{g.root, other}}) {
		t.Error("a workspace bound to another organization shares the conversation's content")
	}
	if paths.workspacesCollected(g.root, &antigravityHookInput{WorkspacePaths: []string{g.root, t.TempDir()}}) {
		t.Error("a workspace outside git, which no selected team collects, shares the conversation's content")
	}
	if !paths.refuses(g.root, "view_file", argsJSON(t, map[string]any{"AbsolutePath": filepath.Join(other, "b.go")})) {
		t.Error("a read in another organization's repository is not refused")
	}
	if paths.refuses(g.root, "view_file", argsJSON(t, map[string]any{"AbsolutePath": filepath.Join(g.root, "a.go")})) {
		t.Error("a read in the hook's own checkout is refused")
	}
}
