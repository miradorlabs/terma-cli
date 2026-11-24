package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func TestGeminiStartRunAndCrossOrgReachMixed(t *testing.T) {
	stateDir := t.TempDir()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	repoA, repoB := filepath.Join(parent, "alpha"), filepath.Join(parent, "beta")
	for _, r := range [][2]string{{repoA, "git@github.com:orgA/alpha.git"}, {repoB, "git@github.com:orgB/beta.git"}} {
		_, _ = gitx.Git(context.Background(), parent, "init", "-q", "-b", "main", r[0])
		_, _ = gitx.Git(context.Background(), parent, "-C", r[0], "remote", "add", "origin", r[1])
	}
	fileB := filepath.Join(repoB, "b.go")
	if err := os.WriteFile(fileB, []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const acct = "1111111111111111"
	now := time.Now().UTC()
	st, err := routing.Create(stateDir, "default", acct, func(s *routing.State) error {
		for _, spec := range [][3]string{{"orgA", "teamA", "github.com/orgA/alpha"}, {"orgB", "teamB", "github.com/orgB/beta"}} {
			if _, err := s.Select(routing.Team{OrganizationID: spec[0], AuthURL: "https://api.example.com", TeamID: spec[1],
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{spec[2]}, Revision: 1, FetchedAt: now, TeamID: spec[1], OrganizationID: spec[0], AuthURL: "https://api.example.com"}}); err != nil {
				return err
			}
			if err := s.Bind(spec[2], spec[0], false); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := spool.Open(filepath.Join(stateDir, spool.Dir))
	sp.Bind(acct)
	env := hookrun.Env{StateDir: stateDir, Cwd: repoA, Spool: sp, Account: acct, Routing: hookrun.NewRouting("default", &st), Now: now}
	ctx := context.Background()
	sid := "018f3a2c-gm01-7000-8000-000000000001"

	t0 := now
	bStart, _ := json.Marshal(map[string]any{"session_id": sid, "cwd": repoA})
	env.Now, env.Stdin = t0, bytes.NewReader(bStart)
	if err := sessionStart(ctx, env); err != nil {
		t.Fatal(err)
	}
	turn1, turn2 := t0.Add(10*time.Second), t0.Add(60*time.Second)
	bPrompt, _ := json.Marshal(map[string]any{"session_id": sid, "cwd": repoA})
	env.Now, env.Stdin = turn1, bytes.NewReader(bPrompt)
	if err := prompt(ctx, env); err != nil {
		t.Fatal(err)
	}
	env.Now, env.Stdin = turn2, bytes.NewReader(bPrompt)
	if err := prompt(ctx, env); err != nil {
		t.Fatal(err)
	}
	bRead, _ := json.Marshal(map[string]any{
		"session_id": sid, "cwd": repoA, "tool_name": "read_file",
		"tool_input": map[string]any{"absolute_path": fileB},
	})
	env.Now, env.Stdin = turn2.Add(5*time.Second), bytes.NewReader(bRead)
	if err := afterTool(ctx, env); err != nil {
		t.Fatal(err)
	}
	c, ok := claim.Read(stateDir, sid, env.Time())
	if !ok || c.Kind != claim.KindMixed {
		t.Fatalf("claim after cross-org read_file = %+v (ok=%v); want KindMixed", c, ok)
	}
	snap := runs.Read(stateDir, sid)
	if _, mixed := snap.MixedInterval(turn1.Add(time.Second), turn1.Add(4*time.Second)); mixed {
		t.Fatal("prior clean turn span was wrongly marked Mixed")
	}
	if at, mixed := snap.MixedInterval(turn2.Add(time.Second), turn2.Add(4*time.Second)); !mixed || at.After(turn2) {
		t.Fatalf("offending tool span [turn2+1s, turn2+4s] not dropped: mixed=%v at=%v want <= %v", mixed, at, turn2)
	}
}
