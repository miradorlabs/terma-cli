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
	"github.com/miradorlabs/terma-cli/internal/routing"
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

// An earlier build registered a PreToolUse hook: refreshing settings.json takes it out, and
// until then the hook it still runs is a silent no-op.
func TestClaudeRefreshDropsTheLegacyPreToolUseHook(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	cmd := hookmgr.UserHookCommand("/usr/local/bin/terma")
	group := func(event string) []any {
		return []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd(event), "timeout": 10}}}}
	}
	raw, err := json.Marshal(map[string]any{"hooks": map[string]any{
		"SessionStart": group("session-start"),
		"PreToolUse":   []any{map[string]any{"matcher": "Read|Grep|Glob|NotebookRead|LS|Bash", "hooks": group("pre-tool-use")[0].(map[string]any)["hooks"]}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if changed, err := refreshUserHooks(path); err != nil || !changed {
		t.Fatalf("refreshUserHooks(legacy) = (%v, %v); want (true, nil)", changed, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "PreToolUse") || strings.Contains(string(b), "pre-tool-use") {
		t.Fatalf("refreshed settings.json still runs the PreToolUse hook:\n%s", b)
	}
	env := hookrun.Env{StateDir: t.TempDir(), Cwd: dir, Stdin: strings.NewReader(`{"session_id":"018f3a2c-cl04-7000-8000-000000000001","tool_name":"Read","tool_input":{"file_path":"/elsewhere/a.go"}}`)}
	if err := (Agent{}).Events()["pre-tool-use"](context.Background(), env); err != nil {
		t.Fatalf("legacy pre-tool-use hook = %v; want nil", err)
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
