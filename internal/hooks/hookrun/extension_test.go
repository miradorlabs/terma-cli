package hookrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
)

func TestExtensionStartRunAndSubagentStart(t *testing.T) {
	stateDir := t.TempDir()
	repoA := namedRepo(t, "alpha", "git@github.com:orgA/alpha.git")
	repoB := namedRepo(t, "beta", "git@github.com:orgB/beta.git")
	fileB := filepath.Join(repoB, "secret.go")
	if err := os.WriteFile(fileB, []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := routedState(t, stateDir, testAccountA, []routing.Team{
		makeTeam("orgA", "team-a", []string{"github.com/orgA/alpha"}, true),
		makeTeam("orgB", "team-b", []string{"github.com/orgB/beta"}, true),
	}, map[string]string{
		"github.com/orgA/alpha": "orgA",
		"github.com/orgB/beta":  "orgB",
	})
	env, _, _ := routedEnv(t, stateDir, repoA, testAccountA, rt)
	ctx := context.Background()
	events := Extension{Tool: "omp", ClaimParent: true}.Events("omp")

	parentID := "018f3a2c-9001-7000-8000-000000000001"
	childID := "018f3a2c-9001-7000-8000-000000000002"
	qA := strconv.Quote(repoA)

	// Non-subagent session-start calls StartRun.
	e1 := env
	e1.Stdin = strings.NewReader(`{"session_id":"` + parentID + `","cwd":` + qA + `}`)
	if err := events["omp-session-start"](ctx, e1); err != nil {
		t.Fatalf("omp-session-start(parent): %v", err)
	}
	run1, ok := runs.Locate(stateDir, parentID)
	if !ok || run1.Run == "" {
		t.Fatalf("parent session-start did not start a run: %+v (ok=%v)", run1, ok)
	}

	// Subagent session-start joins the parent's run (never calls StartRun) and links alias.
	e2 := env
	e2.Stdin = strings.NewReader(`{"session_id":"` + childID + `","cwd":` + qA + `,"parent_session_id":"` + parentID + `"}`)
	if err := events["omp-session-start"](ctx, e2); err != nil {
		t.Fatalf("omp-session-start(child): %v", err)
	}
	if childRun, ok := runs.Locate(stateDir, childID); !ok || childRun.Run != run1.Run {
		t.Fatalf("subagent session-start run = %q (ok=%v); want parent run %q", childRun.Run, ok, run1.Run)
	}
	if c, ok := claim.Read(stateDir, childID, env.Time()); !ok || c.Parent != parentID {
		t.Fatalf("subagent claim = %+v (ok=%v); want Parent=%q", c, ok, parentID)
	}

	// Subagent crossing into another org marks BOTH the subagent and parent Mixed.
	eCross := env
	eCross.Stdin = strings.NewReader(`{"session_id":"` + childID + `","parent_session_id":"` + parentID + `","cwd":` + qA + `,"tool":"read","paths":[` + strconv.Quote(fileB) + `]}`)
	if err := events["omp-tool-reach"](ctx, eCross); err != nil {
		t.Fatalf("omp-tool-reach(child): %v", err)
	}
	if p, ok := runs.Locate(stateDir, parentID); !ok || p.MixedAt.IsZero() {
		t.Fatalf("parent run not Mixed after subagent crossing: %+v (ok=%v)", p, ok)
	}
	if c, ok := claim.Read(stateDir, parentID, env.Time()); !ok || c.Kind != claim.KindMixed {
		t.Fatalf("parent claim after subagent crossing = %+v (ok=%v); want KindMixed", c, ok)
	}
	if c, ok := claim.Read(stateDir, childID, env.Time()); !ok || c.Kind != claim.KindMixed {
		t.Fatalf("child claim after subagent crossing = %+v (ok=%v); want KindMixed", c, ok)
	}

	// Second non-subagent session-start starts a fresh clean run id.
	e3 := env
	e3.Stdin = strings.NewReader(`{"session_id":"` + parentID + `","cwd":` + qA + `}`)
	if err := events["omp-session-start"](ctx, e3); err != nil {
		t.Fatalf("second omp-session-start: %v", err)
	}
	run2, ok := runs.Locate(stateDir, parentID)
	if !ok || run2.Run == "" || run2.Run == run1.Run || !run2.MixedAt.IsZero() {
		t.Fatalf("second session-start run = %+v; want new clean id != %q", run2, run1.Run)
	}
}

func TestExtensionMixedPropagatesParentToChildrenAndSiblings(t *testing.T) {
	stateDir := t.TempDir()
	repoA := namedRepo(t, "alpha", "git@github.com:orgA/alpha.git")
	repoB := namedRepo(t, "beta", "git@github.com:orgB/beta.git")
	fileB := filepath.Join(repoB, "secret.go")
	if err := os.WriteFile(fileB, []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := routedState(t, stateDir, testAccountA, []routing.Team{
		makeTeam("orgA", "team-a", []string{"github.com/orgA/alpha"}, true),
		makeTeam("orgB", "team-b", []string{"github.com/orgB/beta"}, true),
	}, map[string]string{
		"github.com/orgA/alpha": "orgA",
		"github.com/orgB/beta":  "orgB",
	})
	ctx := context.Background()
	events := Extension{Tool: "omp", ClaimParent: true}.Events("omp")
	call := func(name string, payload map[string]any) {
		t.Helper()
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		e, _, _ := routedEnv(t, stateDir, repoA, testAccountA, LoadRouting(stateDir, "default"))
		e.Stdin = strings.NewReader(string(b))
		if err := events[name](ctx, e); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	_ = rt

	// Case 1: parent crosses orgs -> parent AND both children become KindMixed.
	p1, c1a, c1b := "018f3a2c-9010-7000-8000-000000000001", "018f3a2c-9010-7000-8000-000000000002", "018f3a2c-9010-7000-8000-000000000003"
	call("omp-session-start", map[string]any{"session_id": p1, "cwd": repoA})
	call("omp-session-start", map[string]any{"session_id": c1a, "parent_session_id": p1, "cwd": repoA})
	call("omp-session-start", map[string]any{"session_id": c1b, "parent_session_id": p1, "cwd": repoA})
	call("omp-tool-reach", map[string]any{"session_id": p1, "cwd": repoA, "tool": "read", "paths": []string{fileB}})
	for _, id := range []string{p1, c1a, c1b} {
		if c, ok := claim.Read(stateDir, id, rt.state.Teams[0].Policy.FetchedAt); !ok || c.Kind != claim.KindMixed {
			t.Fatalf("parent-cross: claim(%s) = %+v (ok=%v); want KindMixed", id, c, ok)
		}
	}

	// Case 2: child1 crosses orgs -> parent, child1, AND sibling child2 ALL become KindMixed.
	p2, c2a, c2b := "018f3a2c-9011-7000-8000-000000000001", "018f3a2c-9011-7000-8000-000000000002", "018f3a2c-9011-7000-8000-000000000003"
	call("omp-session-start", map[string]any{"session_id": p2, "cwd": repoA})
	call("omp-session-start", map[string]any{"session_id": c2a, "parent_session_id": p2, "cwd": repoA})
	call("omp-session-start", map[string]any{"session_id": c2b, "parent_session_id": p2, "cwd": repoA})
	call("omp-tool-reach", map[string]any{"session_id": c2a, "parent_session_id": p2, "cwd": repoA, "tool": "read", "paths": []string{fileB}})
	for _, id := range []string{p2, c2a, c2b} {
		if c, ok := claim.Read(stateDir, id, rt.state.Teams[0].Policy.FetchedAt); !ok || c.Kind != claim.KindMixed {
			t.Fatalf("sibling-cross: claim(%s) = %+v (ok=%v); want KindMixed", id, c, ok)
		}
	}
}

func TestExtensionToolReachCrossOrgMixed(t *testing.T) {
	stateDir := t.TempDir()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repoA := filepath.Join(parent, "alpha")
	repoB := filepath.Join(parent, "beta")
	for _, r := range [][2]string{
		{repoA, "git@github.com:orgA/alpha.git"},
		{repoB, "git@github.com:orgB/beta.git"},
	} {
		gitRun(t, parent, "init", "-q", "-b", "main", r[0])
		gitRun(t, r[0], "remote", "add", "origin", r[1])
	}
	fileA := filepath.Join(repoA, "a.go")
	fileB := filepath.Join(repoB, "secret.go")
	for _, f := range []string{fileA, fileB} {
		if err := os.WriteFile(f, []byte("package p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rt := routedState(t, stateDir, testAccountA, []routing.Team{
		makeTeam("orgA", "team-a", []string{"github.com/orgA/alpha"}, true),
		makeTeam("orgB", "team-b", []string{"github.com/orgB/beta"}, true),
	}, map[string]string{
		"github.com/orgA/alpha": "orgA",
		"github.com/orgB/beta":  "orgB",
	})
	env, _, _ := routedEnv(t, stateDir, repoA, testAccountA, rt)
	ctx := context.Background()
	events := Extension{Tool: "pi"}.Events("pi")

	cases := []struct {
		name      string
		payload   map[string]any
		wantKind  claim.Kind
		wantMixed bool
	}{
		{
			name:     "same repo read stays clean",
			payload:  map[string]any{"cwd": repoA, "tool": "read", "paths": []string{fileA}},
			wantKind: claim.KindRepository,
		},
		{
			name:      "cross-org read marks mixed",
			payload:   map[string]any{"cwd": repoA, "tool": "read", "paths": []string{fileB}},
			wantKind:  claim.KindMixed,
			wantMixed: true,
		},
		{
			name:      "cross-org grep marks mixed",
			payload:   map[string]any{"cwd": repoA, "tool": "grep", "paths": []string{repoB}, "globs": []string{"*.go"}},
			wantKind:  claim.KindMixed,
			wantMixed: true,
		},
		{
			name:      "cross-org glob marks mixed",
			payload:   map[string]any{"cwd": repoA, "tool": "glob", "globs": []string{"../beta/**/*.go"}},
			wantKind:  claim.KindMixed,
			wantMixed: true,
		},
		{
			name:      "cross-org list marks mixed",
			payload:   map[string]any{"cwd": repoA, "tool": "ls", "lists": []string{repoB}},
			wantKind:  claim.KindMixed,
			wantMixed: true,
		},
		{
			name:      "cross-org bash command marks mixed",
			payload:   map[string]any{"cwd": repoA, "tool": "bash", "command": "cat ../beta/secret.go"},
			wantKind:  claim.KindMixed,
			wantMixed: true,
		},
		{
			name:      "cross-org bash dir marks mixed",
			payload:   map[string]any{"cwd": repoA, "tool": "bash", "command": "pwd", "dir": repoB},
			wantKind:  claim.KindMixed,
			wantMixed: true,
		},
		{
			name:      "unknown shell reach marks mixed",
			payload:   map[string]any{"cwd": repoA, "tool": "bash", "command": `eval "$DYNAMIC_CMD"`},
			wantKind:  claim.KindMixed,
			wantMixed: true,
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sid := "018f3a2c-9002-7000-8000-00000000000" + string(rune('1'+i))
			eStart := env
			eStart.Stdin = strings.NewReader(`{"session_id":"` + sid + `","cwd":` + strconv.Quote(repoA) + `}`)
			if err := events["pi-session-start"](ctx, eStart); err != nil {
				t.Fatalf("session-start: %v", err)
			}
			tc.payload["session_id"] = sid
			body, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			eReach := env
			eReach.Stdin = strings.NewReader(string(body))
			if err := events["pi-tool-reach"](ctx, eReach); err != nil {
				t.Fatalf("tool-reach: %v", err)
			}
			c, ok := claim.Read(stateDir, sid, env.Time())
			if !ok || c.Kind != tc.wantKind {
				t.Fatalf("claim = %+v (ok=%v); want Kind=%v", c, ok, tc.wantKind)
			}
			p, _ := runs.Locate(stateDir, sid)
			if gotMixed := !p.MixedAt.IsZero(); gotMixed != tc.wantMixed {
				t.Fatalf("runs.Locate MixedAt=%v; want mixed=%v", p.MixedAt, tc.wantMixed)
			}
		})
	}
}

func TestExtensionToolReachIgnoresBadInput(t *testing.T) {
	t.Parallel()
	events := Extension{Tool: "pi"}.Events("pi")
	for _, stdin := range []string{"", "not json", `{"cwd":"/x"}`, `{"session_id":"../etc","paths":["/x"]}`} {
		env := Env{StateDir: t.TempDir(), Cwd: t.TempDir(), Stdin: strings.NewReader(stdin)}
		if err := events["pi-tool-reach"](context.Background(), env); err != nil {
			t.Errorf("tool-reach(%q) = %v; want nil", stdin, err)
		}
	}
}
