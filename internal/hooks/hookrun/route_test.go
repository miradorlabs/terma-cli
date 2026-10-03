package hookrun

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// A hook reads the routing record once, on first use: what it decided at the start of
// the hook holds to the end of it.
func TestRouteIsReadOnceOnFirstUse(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	if err := routing.SaveRecord(routing.Record{ProjectID: "p1", Signals: []string{"logs"}}); err != nil {
		t.Fatal(err)
	}
	r := &Repo{ProjectID: "p1"}
	rec, recorded, err := r.Route()
	if err != nil || !recorded || len(rec.Signals) != 1 {
		t.Fatalf("Route = %+v, %v, %v", rec, recorded, err)
	}
	if err := routing.SaveRecord(routing.Record{ProjectID: "p1"}); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := r.Route(); len(rec.Signals) != 1 {
		t.Fatal("a second Route read the record again")
	}
	if c := r.Consent(true); !c.Recorded || len(c.Route.Signals) != 1 || !c.Global {
		t.Fatalf("Consent = %+v", c)
	}
}

// Resolving a repository never reads its routing record: prepare-commit-msg resolves one
// on every commit and has no use for it.
func TestResolvingARepositoryLeavesTheRouteUnread(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := Env{Cwd: root}.Repo(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if r.route != nil {
		t.Fatal("Repo read the routing record")
	}
}

// A record that exists and cannot be read is an error, never "not recorded": it may be
// the one that withholds content.
func TestAnUnreadableRouteIsNeverAbsent(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	dir, err := routing.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "p1.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Repo{ProjectID: "p1"}
	if _, recorded, err := r.Route(); err == nil || recorded {
		t.Fatalf("Route = recorded %v, err %v", recorded, err)
	}
	if c := r.Consent(true); c.RouteErr == nil {
		t.Fatalf("Consent = %+v", c)
	}
	if c := ConsentFor("p1", true); c.RouteErr == nil {
		t.Fatalf("ConsentFor = %+v", c)
	}
}

// Hooks know no policy: an event whose shell command names an excluded file is spooled
// whole, with the directory the hook ran in, which delivery resolves the command against.
func TestEmitForSpoolsWholeWithWhereTheHookRan(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	root := t.TempDir()
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := Env{Spool: sp, Policy: config.Policy{Mode: config.ModeRepo, ExcludePaths: []string{"secrets/**"}}}
	r := &Repo{Root: root, ProjectID: "p1"}
	for _, dir := range []string{"secrets", "src"} {
		env.Cwd = filepath.Join(root, dir)
		env.EmitFor(r, spool.Event{Name: EventToolCall, SessionID: "s1",
			Attrs: map[string]any{"arguments": `{"command":["bash","-lc","cat app.env"]}`}})
	}
	got := hookruntest.Spooled(t, sp)
	if len(got) != 2 || got[0].Cwd != filepath.Join(root, "secrets") || got[1].Cwd != filepath.Join(root, "src") || got[0].Workspace != root {
		t.Fatalf("spooled %+v, want both calls whole, each with its cwd", got)
	}
}
