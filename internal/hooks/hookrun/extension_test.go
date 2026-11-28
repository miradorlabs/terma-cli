package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// An extension's subagent session-start claims the child naming the session that opened it,
// and a tool-reach from a plugin an earlier build installed claims nothing, writes nothing
// and succeeds.
func TestExtensionSubagentStartAndLegacyToolReach(t *testing.T) {
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
	})
	env, _, _ := routedEnv(t, stateDir, repoA, testAccountA, rt)
	ctx := context.Background()
	events := Extension{Tool: "omp", ClaimParent: true}.Events("omp")

	parentID := "018f3a2c-9001-7000-8000-000000000001"
	childID := "018f3a2c-9001-7000-8000-000000000002"
	qA := strconv.Quote(repoA)

	e1 := env
	e1.Stdin = strings.NewReader(`{"session_id":"` + parentID + `","cwd":` + qA + `}`)
	if err := events["omp-session-start"](ctx, e1); err != nil {
		t.Fatalf("omp-session-start(parent): %v", err)
	}
	e2 := env
	e2.Stdin = strings.NewReader(`{"session_id":"` + childID + `","cwd":` + qA + `,"parent_session_id":"` + parentID + `"}`)
	if err := events["omp-session-start"](ctx, e2); err != nil {
		t.Fatalf("omp-session-start(child): %v", err)
	}
	child, ok := claim.Read(stateDir, childID, env.Time())
	if !ok || child.Parent != parentID {
		t.Fatalf("subagent claim = %+v (ok=%v); want Parent=%q", child, ok, parentID)
	}
	parent, _ := claim.Read(stateDir, parentID, env.Time())

	eReach := env
	eReach.Stdin = strings.NewReader(`{"session_id":"` + childID + `","parent_session_id":"` + parentID + `","cwd":` + qA + `,"tool":"read","paths":[` + strconv.Quote(fileB) + `]}`)
	if err := events["omp-tool-reach"](ctx, eReach); err != nil {
		t.Fatalf("omp-tool-reach: %v", err)
	}
	if c, _ := claim.Read(stateDir, childID, env.Time()); c.Repository != child.Repository || len(c.Placements) != len(child.Placements) {
		t.Fatalf("omp-tool-reach changed the child's claim: %+v, was %+v", c, child)
	}
	if c, _ := claim.Read(stateDir, parentID, env.Time()); c.Repository != parent.Repository || len(c.Placements) != len(parent.Placements) {
		t.Fatalf("omp-tool-reach changed the parent's claim: %+v, was %+v", c, parent)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "runs")); !os.IsNotExist(err) {
		t.Fatalf("omp-tool-reach wrote run state: %v", err)
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
