package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func initCodexOriginRepo(t *testing.T, dirName, remote string) string {
	t.Helper()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	root := filepath.Join(parent, dirName)
	for _, a := range [][]string{
		{"init", "-q", "-b", "main", root},
		{"-C", root, "remote", "add", "origin", remote},
	} {
		if _, err := gitx.Git(context.Background(), parent, a...); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func makeRoutedCodexEnv(t *testing.T, stateDir, configDir, cwd string) (hookrun.Env, map[string]string) {
	t.Helper()
	const acct = "1111111111111111"
	now := time.Now().UTC()
	enrollments := map[string]string{}
	st, err := routing.Create(stateDir, "default", acct, func(s *routing.State) error {
		for _, spec := range []struct{ org, team, repo string }{
			{"orgA", "teamA", "github.com/orgA/alpha"},
			{"orgB", "teamB", "github.com/orgB/beta"},
		} {
			enr, err := s.Select(routing.Team{
				OrganizationID: spec.org, AuthURL: "https://api.example.com", TeamID: spec.team,
				Policy: config.Policy{
					Mode: config.ModeRepo, Repositories: []string{spec.repo},
					IncludePrompts: true, IncludeToolContent: true, Revision: 1,
					FetchedAt: now, TeamID: spec.team, OrganizationID: spec.org, AuthURL: "https://api.example.com",
				},
			})
			if err != nil {
				return err
			}
			enrollments[spec.org] = enr
			if err := s.Bind(spec.repo, spec.org, false); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.Open(filepath.Join(stateDir, spool.Dir))
	if err != nil {
		t.Fatal(err)
	}
	sp.Bind(acct)
	return hookrun.Env{
		ConfigDir: configDir, StateDir: stateDir, Cwd: cwd, Spool: sp,
		Account: acct, Agents: []string{name}, Routing: hookrun.NewRouting("default", &st), Now: now,
	}, enrollments
}

func TestCodexReachCrossOrgMixedAndPerTurnRouting(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	stateDir, configDir := t.TempDir(), t.TempDir()
	repoA := initCodexOriginRepo(t, "alpha", "git@github.com:orgA/alpha.git")
	repoB := initCodexOriginRepo(t, "beta", "git@github.com:orgB/beta.git")
	repoUn := initCodexOriginRepo(t, "uncollected", "git@github.com:other/uncollected.git")
	fileB := filepath.Join(repoB, "b.go")
	if err := os.WriteFile(fileB, []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	env, enrollments := makeRoutedCodexEnv(t, stateDir, configDir, repoA)
	ctx := context.Background()

	// 1. Cross-org shell reach in PreToolUse marks session Mixed.
	sid := "018f3a2c-cx01-7000-8000-000000000001"
	startJSON, _ := json.Marshal(map[string]any{"session_id": sid, "cwd": repoA})
	env.Stdin = bytes.NewReader(startJSON)
	if err := sessionStart(ctx, env); err != nil {
		t.Fatal(err)
	}
	preJSON, _ := json.Marshal(map[string]any{
		"session_id": sid, "cwd": repoA, "tool_name": codexShellTool,
		"tool_input": map[string]any{"command": "cat " + fileB},
	})
	env.Stdin = bytes.NewReader(preJSON)
	if err := preToolUse(ctx, env); err != nil {
		t.Fatal(err)
	}
	if c, ok := claim.Read(stateDir, sid, env.Time()); !ok || c.Kind != claim.KindMixed {
		t.Fatalf("claim after cross-org Bash = %+v (ok=%v); want KindMixed", c, ok)
	}

	// 2. Per-turn routing: rollout with turns in repoA, repoUn (withheld), and repoB routes each reply to its own enrollment.
	sid2 := "018f3a2c-cx02-7000-8000-000000000002"
	turnA, turnU, turnB := "01a0bae4-aaaa-7910-9022-15897b3021a1", "01a0bae4-uuuu-7910-9022-15897b3021a2", "01a0bae4-bbbb-7910-9022-15897b3021a3"
	rollout := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "09", "19", "rollout-2026-09-19T12-18-12-"+sid2+".jsonl")
	hookruntest.WriteFile(t, filepath.Dir(rollout), filepath.Base(rollout), strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"` + sid2 + `","cwd":` + strconv.Quote(repoA) + `}}`,
		`{"timestamp":"2026-09-19T18:18:39.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"` + turnA + `","trace_id":"ae2a5e6f8e0168ae5ec2efa2c8772b01"}}`,
		strings.TrimSuffix(turnContext(turnA, repoA), "\n"),
		`{"timestamp":"2026-09-19T18:18:41.000Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg_a","phase":"final_answer","content":[{"type":"output_text","text":"reply in A"}]}}`,
		`{"timestamp":"2026-09-19T18:18:42.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"` + turnA + `"}}`,
		`{"timestamp":"2026-09-19T18:19:00.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"` + turnU + `","trace_id":"ae2a5e6f8e0168ae5ec2efa2c8772b02"}}`,
		strings.TrimSuffix(turnContext(turnU, repoUn), "\n"),
		`{"timestamp":"2026-09-19T18:19:01.000Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg_u","phase":"final_answer","content":[{"type":"output_text","text":"reply in uncollected"}]}}`,
		`{"timestamp":"2026-09-19T18:19:02.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"` + turnU + `"}}`,
		`{"timestamp":"2026-09-19T18:20:00.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"` + turnB + `","trace_id":"ae2a5e6f8e0168ae5ec2efa2c8772b03"}}`,
		strings.TrimSuffix(turnContext(turnB, repoB), "\n"),
		`{"timestamp":"2026-09-19T18:20:01.000Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg_b","phase":"final_answer","content":[{"type":"output_text","text":"reply in B"}]}}`,
		`{"timestamp":"2026-09-19T18:20:02.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"` + turnB + `"}}`,
	}, "\n")+"\n")

	env.Cwd = repoB
	stopJSON, _ := json.Marshal(map[string]any{"session_id": sid2, "cwd": repoB, "transcript_path": rollout, "model": "gpt-5.6"})
	env.Stdin = bytes.NewReader(stopJSON)
	if err := stop(ctx, env); err != nil {
		t.Fatal(err)
	}
	byText := map[string]spool.Event{}
	for _, ev := range hookruntest.Spooled(t, env.Spool) {
		if ev.Name == semconv.TermaAssistantMessageEvent {
			text, _ := ev.Attrs[semconv.TermaMessageTextKey].(string)
			byText[text] = ev
		}
	}
	if len(byText) != 2 {
		t.Fatalf("spooled replies = %v; want exactly 2 (repoA and repoB, not uncollected)", byText)
	}
	if byText["reply in A"].Enrollment != enrollments["orgA"] || !byText["reply in A"].OwnLocation {
		t.Fatalf("turn A event = %+v; want enrollment %q with OwnLocation=true", byText["reply in A"], enrollments["orgA"])
	}
	if byText["reply in B"].Enrollment != enrollments["orgB"] || !byText["reply in B"].OwnLocation {
		t.Fatalf("turn B event = %+v; want enrollment %q with OwnLocation=true", byText["reply in B"], enrollments["orgB"])
	}
}

func TestCodexResumedCrossOrgWithholdsThreadTitle(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	stateDir, configDir := t.TempDir(), t.TempDir()
	repoA := initCodexOriginRepo(t, "alpha", "git@github.com:orgA/alpha.git")
	repoB := initCodexOriginRepo(t, "beta", "git@github.com:orgB/beta.git")
	env, enrollments := makeRoutedCodexEnv(t, stateDir, configDir, repoA)
	ctx := context.Background()

	sid := "018f3a2c-cx03-7000-8000-000000000003"
	turnA, turnB := "01a0bae4-aaaa-7910-9022-15897b3022a1", "01a0bae4-bbbb-7910-9022-15897b3022b2"
	rollout := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "09", "19", "rollout-2026-09-19T12-18-12-"+sid+".jsonl")
	titleIndex(t, `{"id":"`+sid+`","thread_name":"Secret Org A Prompt","updated_at":"2026-09-19T18:18:45Z"}`)

	// Turn 1 in orgA -> stop in orgA queues the title for orgA.
	turn1 := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"` + sid + `","cwd":` + strconv.Quote(repoA) + `}}`,
		`{"timestamp":"2026-09-19T18:18:39.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"` + turnA + `","trace_id":"ae2a5e6f8e0168ae5ec2efa2c8772c01"}}`,
		strings.TrimSuffix(turnContext(turnA, repoA), "\n"),
		`{"timestamp":"2026-09-19T18:18:41.000Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg_a","phase":"final_answer","content":[{"type":"output_text","text":"reply in A"}]}}`,
		`{"timestamp":"2026-09-19T18:18:42.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"` + turnA + `"}}`,
	}, "\n") + "\n"
	hookruntest.WriteFile(t, filepath.Dir(rollout), filepath.Base(rollout), turn1)
	stopA, _ := json.Marshal(map[string]any{"session_id": sid, "cwd": repoA, "transcript_path": rollout, "model": "gpt-5.6"})
	env.Cwd, env.Stdin = repoA, bytes.NewReader(stopA)
	if err := stop(ctx, env); err != nil {
		t.Fatal(err)
	}

	// Resume thread in orgB (Turn 2) -> stop in orgB must NOT spool the orgA-derived thread title to orgB.
	turn2 := turn1 + strings.Join([]string{
		`{"timestamp":"2026-09-19T18:20:00.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"` + turnB + `","trace_id":"ae2a5e6f8e0168ae5ec2efa2c8772c02"}}`,
		strings.TrimSuffix(turnContext(turnB, repoB), "\n"),
		`{"timestamp":"2026-09-19T18:20:01.000Z","type":"response_item","payload":{"type":"message","role":"assistant","id":"msg_b","phase":"final_answer","content":[{"type":"output_text","text":"reply in B"}]}}`,
		`{"timestamp":"2026-09-19T18:20:02.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"` + turnB + `"}}`,
	}, "\n") + "\n"
	hookruntest.WriteFile(t, filepath.Dir(rollout), filepath.Base(rollout), turn2)
	stopB, _ := json.Marshal(map[string]any{"session_id": sid, "cwd": repoB, "transcript_path": rollout, "model": "gpt-5.6"})
	env.Routing = hookrun.LoadRouting(stateDir, "default")
	env.Cwd, env.Stdin = repoB, bytes.NewReader(stopB)
	if err := stop(ctx, env); err != nil {
		t.Fatal(err)
	}

	titles := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSessionTitleEvent)
	if len(titles) != 1 || titles[0].Enrollment != enrollments["orgA"] {
		t.Fatalf("spooled titles = %+v; want exactly 1 title for orgA (%q) and none for orgB (%q)", titles, enrollments["orgA"], enrollments["orgB"])
	}
}
