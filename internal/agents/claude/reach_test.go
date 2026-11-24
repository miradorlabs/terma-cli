package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func initOriginRepo(t *testing.T, name, remote string) string {
	t.Helper()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	root := filepath.Join(parent, name)
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

func makeRoutedClaudeEnv(t *testing.T, stateDir, cwd string) (hookrun.Env, *hookrun.Routing) {
	t.Helper()
	const acct = "1111111111111111"
	now := time.Now().UTC()
	st, err := routing.Create(stateDir, "default", acct, func(s *routing.State) error {
		for _, spec := range []struct{ org, team, repo string }{
			{"orgA", "teamA", "github.com/orgA/alpha"},
			{"orgB", "teamB", "github.com/orgB/beta"},
		} {
			if _, err := s.Select(routing.Team{
				OrganizationID: spec.org, AuthURL: "https://api.example.com", TeamID: spec.team,
				Policy: config.Policy{
					Mode: config.ModeRepo, Repositories: []string{spec.repo},
					IncludePrompts: true, IncludeToolContent: true, Revision: 1,
					FetchedAt: now, TeamID: spec.team, OrganizationID: spec.org, AuthURL: "https://api.example.com",
				},
			}); err != nil {
				return err
			}
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
	rt := hookrun.NewRouting("default", &st)
	return hookrun.Env{
		StateDir: stateDir, Cwd: cwd, Spool: sp, Account: acct, Routing: rt, Now: now,
	}, rt
}

func TestClaudePreToolUseAndStartRunCrossOrgMixed(t *testing.T) {
	stateDir := t.TempDir()
	repoA := initOriginRepo(t, "alpha", "git@github.com:orgA/alpha.git")
	repoB := initOriginRepo(t, "beta", "git@github.com:orgB/beta.git")
	fileB := filepath.Join(repoB, "secret.go")
	if err := os.WriteFile(fileB, []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env, _ := makeRoutedClaudeEnv(t, stateDir, repoA)
	ctx := context.Background()
	sid := "018f3a2c-cl01-7000-8000-000000000001"

	// SessionStart starts a run.
	startJSON, _ := json.Marshal(map[string]any{"session_id": sid, "cwd": repoA, "source": "startup"})
	env.Stdin = bytes.NewReader(startJSON)
	if err := sessionStart(ctx, env); err != nil {
		t.Fatal(err)
	}
	p1, _ := runs.Locate(stateDir, sid)
	if p1.Run == "" {
		t.Fatal("SessionStart did not start a run")
	}

	// Compaction SessionStart must NOT reset the run.
	compactJSON, _ := json.Marshal(map[string]any{"session_id": sid, "cwd": repoA, "source": "compact"})
	env.Stdin = bytes.NewReader(compactJSON)
	if err := sessionStart(ctx, env); err != nil {
		t.Fatal(err)
	}
	if p2, _ := runs.Locate(stateDir, sid); p2.Run != p1.Run {
		t.Fatalf("compact SessionStart changed run from %q to %q", p1.Run, p2.Run)
	}

	// PreToolUse Read into orgB marks the run Mixed and writes NOTHING to stdout.
	var stdout bytes.Buffer
	preJSON, _ := json.Marshal(map[string]any{
		"session_id": sid, "cwd": repoA, "tool_name": "Read",
		"tool_input": map[string]any{"file_path": fileB},
	})
	env.Stdin = bytes.NewReader(preJSON)
	env.Stdout = &stdout
	if err := preToolUse(ctx, env); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("PreToolUse wrote stdout %q; must print nothing", stdout.String())
	}
	c, ok := claim.Read(stateDir, sid, env.Time())
	if !ok || c.Kind != claim.KindMixed {
		t.Fatalf("claim after cross-org Read = %+v (ok=%v); want KindMixed", c, ok)
	}

	// Bash reach in a fresh session also marks Mixed.
	sid2 := "018f3a2c-cl01-7000-8000-000000000002"
	startJSON2, _ := json.Marshal(map[string]any{"session_id": sid2, "cwd": repoA, "source": "startup"})
	env.Stdin = bytes.NewReader(startJSON2)
	_ = sessionStart(ctx, env)
	bashJSON, _ := json.Marshal(map[string]any{
		"session_id": sid2, "cwd": repoA, "tool_name": "Bash",
		"tool_input": map[string]any{"command": "cat " + fileB},
	})
	env.Stdin = bytes.NewReader(bashJSON)
	if err := preToolUse(ctx, env); err != nil {
		t.Fatal(err)
	}
	c2, ok := claim.Read(stateDir, sid2, env.Time())
	if !ok || c2.Kind != claim.KindMixed {
		t.Fatalf("claim after cross-org Bash = %+v (ok=%v); want KindMixed", c2, ok)
	}
}

func TestClaudePreToolUseRegisteredAndRefreshedInUserHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	// Unhooked settings file: refreshUserHooks does nothing.
	if err := os.WriteFile(path, []byte(`{"env":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := refreshUserHooks(path); err != nil || changed {
		t.Fatalf("refreshUserHooks(unhooked) = (%v, %v); want (false, nil)", changed, err)
	}
	// Pre-PR8 settings file with only SessionStart: refreshUserHooks adds PreToolUse.
	oldCmd := hookmgr.UserHookCommand("/usr/local/bin/terma")("session-start")
	oldDoc := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{"hooks": []any{
				map[string]any{"type": "command", "command": oldCmd, "timeout": 10},
			}}},
		},
	}
	raw, err := json.Marshal(oldDoc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := refreshUserHooks(path)
	if err != nil || !changed {
		t.Fatalf("refreshUserHooks(upgrade) = (%v, %v); want (true, nil)", changed, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "PreToolUse") || !strings.Contains(string(b), "pre-tool-use") {
		t.Fatalf("refreshed settings.json missing PreToolUse hook:\n%s", b)
	}
}

func TestClaudeRoutedSkillBacklogDoesNotLeakAcrossOrgs(t *testing.T) {
	const acct = "1111111111111111"
	stateDir := t.TempDir()
	repoA := initOriginRepo(t, "alpha", "git@github.com:orgA/alpha.git")
	repoB := initOriginRepo(t, "beta", "git@github.com:orgB/beta.git")
	hookruntest.WriteFile(t, repoA, ".claude/skills/secret-a/SKILL.md", "---\nname: secret-a\ndescription: Org A only.\n---\n\nbody\n")
	hookruntest.WriteFile(t, repoB, ".claude/skills/public-b/SKILL.md", "---\nname: public-b\ndescription: Org B ok.\n---\n\nbody\n")

	now := time.Now().UTC()
	enrollments := map[string]string{}
	st, err := routing.Create(stateDir, "default", acct, func(s *routing.State) error {
		for _, spec := range []struct {
			org, team, repo string
			collect         bool
		}{
			{"orgA", "teamA", "github.com/orgA/alpha", false},
			{"orgB", "teamB", "github.com/orgB/beta", true},
		} {
			enr, err := s.Select(routing.Team{
				OrganizationID: spec.org, AuthURL: "https://api.example.com", TeamID: spec.team,
				Policy: config.Policy{
					Mode: config.ModeRepo, Repositories: []string{spec.repo},
					IncludePrompts: spec.collect, IncludeToolContent: spec.collect, Revision: 1,
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

	cwdA, _ := json.Marshal(repoA)
	cwdB, _ := json.Marshal(repoB)
	baseDirA, _ := json.Marshal("Base directory for this skill: " + filepath.Join(repoA, ".claude", "skills", "secret-a") + "\n")
	baseDirB, _ := json.Marshal("Base directory for this skill: " + filepath.Join(repoB, ".claude", "skills", "public-b") + "\n")
	transcript := filepath.Join(t.TempDir(), "claude.jsonl")
	lines := []string{
		`{"type":"assistant","cwd":` + string(cwdA) + `,"message":{"content":[{"type":"tool_use","id":"toolu_a","name":"Skill","input":{"skill":"secret-a"}}]}}`,
		`{"type":"user","isMeta":true,"cwd":` + string(cwdA) + `,"sourceToolUseID":"toolu_a","message":{"content":[{"type":"text","text":` + string(baseDirA) + `}]}}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	sid := "018f3a2c-cl02-7000-8000-000000000002"
	env := hookrun.Env{StateDir: stateDir, Cwd: repoA, Spool: sp, Account: acct, Routing: hookrun.NewRouting("default", &st), Now: now}
	rA, err := env.Repo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Stop in restrictive orgA leaves the transcript skill cursor unadvanced.
	captureClaudeSkills(ctx, env, rA, sid, transcript)

	// Session resumes in permissive orgB and uses an orgB skill: orgA's unread skill must NOT be spooled to orgB.
	lines = append(lines,
		`{"type":"assistant","cwd":`+string(cwdB)+`,"message":{"content":[{"type":"tool_use","id":"toolu_b","name":"Skill","input":{"skill":"public-b"}}]}}`,
		`{"type":"user","isMeta":true,"cwd":`+string(cwdB)+`,"sourceToolUseID":"toolu_b","message":{"content":[{"type":"text","text":`+string(baseDirB)+`}]}}`,
	)
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.Cwd = repoB
	env.Routing = hookrun.LoadRouting(stateDir, "default")
	rB, err := env.Repo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	captureClaudeSkills(ctx, env, rB, sid, transcript)

	got := hookruntest.Named(hookruntest.Spooled(t, sp), semconv.TermaSkillDefinitionEvent)
	if len(got) != 1 || got[0].Attrs[semconv.TermaSkillNameKey] != "public-b" || got[0].Enrollment != enrollments["orgB"] {
		t.Fatalf("spooled skills = %+v; want only public-b for orgB (%q)", got, enrollments["orgB"])
	}
}

func TestClaudeRoutedTitleWithheldAcrossDestinations(t *testing.T) {
	stateDir := t.TempDir()
	repoA := initOriginRepo(t, "alpha", "git@github.com:orgA/alpha.git")
	repoB := initOriginRepo(t, "beta", "git@github.com:orgB/beta.git")
	env, _ := makeRoutedClaudeEnv(t, stateDir, repoB)
	env.Agents = []string{name}
	ctx := context.Background()
	cwdA, _ := json.Marshal(repoA)
	cwdB, _ := json.Marshal(repoB)

	// Session begins in orgA, produces an ai-title before capture, then resumes cleanly in orgB.
	sidCross := "018f3a2c-cl03-7000-8000-000000000001"
	startB, _ := json.Marshal(map[string]any{"session_id": sidCross, "cwd": repoB, "source": "resume"})
	env.Stdin = bytes.NewReader(startB)
	if err := sessionStart(ctx, env); err != nil {
		t.Fatal(err)
	}
	crossPath := filepath.Join(t.TempDir(), "cross.jsonl")
	crossLines := strings.Join([]string{
		`{"type":"user","sessionId":"` + sidCross + `","cwd":` + string(cwdA) + `}`,
		`{"type":"ai-title","sessionId":"` + sidCross + `","aiTitle":"Org A secret prompt"}`,
		`{"type":"user","sessionId":"` + sidCross + `","cwd":` + string(cwdB) + `}`,
	}, "\n") + "\n"
	if err := os.WriteFile(crossPath, []byte(crossLines), 0o600); err != nil {
		t.Fatal(err)
	}
	stopB, _ := json.Marshal(map[string]any{"session_id": sidCross, "cwd": repoB, "transcript_path": crossPath})
	env.Stdin = bytes.NewReader(stopB)
	if err := stop(ctx, env); err != nil {
		t.Fatal(err)
	}
	if got := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSessionTitleEvent); len(got) != 0 {
		t.Fatalf("cross-org transcript spooled title to orgB: %+v", got)
	}

	// Transcript without cwd provenance fails closed under routing.
	sidNoCwd := "018f3a2c-cl03-7000-8000-000000000002"
	noCwdPath := filepath.Join(t.TempDir(), "nocwd.jsonl")
	if err := os.WriteFile(noCwdPath, []byte(`{"type":"ai-title","sessionId":"`+sidNoCwd+`","aiTitle":"Unknown origin"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stopNoCwd, _ := json.Marshal(map[string]any{"session_id": sidNoCwd, "cwd": repoB, "transcript_path": noCwdPath})
	env.Stdin = bytes.NewReader(stopNoCwd)
	if err := stop(ctx, env); err != nil {
		t.Fatal(err)
	}
	if got := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSessionTitleEvent); len(got) != 0 {
		t.Fatalf("provenance-less transcript spooled title: %+v", got)
	}

	// Single-destination orgB transcript spools its title exclusively to orgB.
	sidPure := "018f3a2c-cl03-7000-8000-000000000003"
	purePath := filepath.Join(t.TempDir(), "pure.jsonl")
	pureLines := strings.Join([]string{
		`{"type":"user","sessionId":"` + sidPure + `","cwd":` + string(cwdB) + `}`,
		`{"type":"ai-title","sessionId":"` + sidPure + `","aiTitle":"Org B pure title"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(purePath, []byte(pureLines), 0o600); err != nil {
		t.Fatal(err)
	}
	stopPure, _ := json.Marshal(map[string]any{"session_id": sidPure, "cwd": repoB, "transcript_path": purePath})
	env.Stdin = bytes.NewReader(stopPure)
	if err := stop(ctx, env); err != nil {
		t.Fatal(err)
	}
	got := hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSessionTitleEvent)
	if len(got) != 1 || got[0].Attrs[semconv.TermaSessionTitleKey] != "Org B pure title" || got[0].SessionID != sidPure {
		t.Fatalf("pure orgB titles = %+v; want 1 title for %s", got, sidPure)
	}
}
