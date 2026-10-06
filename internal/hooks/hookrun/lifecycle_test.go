package hookrun

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// A session edits files in its own repository, another the team collects, one it does not
// and a folder outside git: each collected repository gets its own event, naming it, and
// only the session's own repository records a manifest and claims the session.
func TestTouchReportsEachFileUnderItsOwnRepository(t *testing.T) {
	t.Parallel()
	own, other, denied := hookruntest.InitRepo(t), hookruntest.InitRepo(t), hookruntest.InitRepo(t)
	outside := t.TempDir()
	stateDir := t.TempDir()
	sp, _ := spool.Open(t.TempDir())
	var claimed []string
	env := Env{
		StateDir: stateDir, Now: time.Now(), Cwd: own, Spool: sp, Team: hookruntest.Team,
		Policy:  listing("github.com/acme/"+filepath.Base(own), "github.com/acme/"+filepath.Base(other)),
		OnClaim: func(root string) { claimed = append(claimed, root) },
	}
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	const sid = "sess-touch-1"
	env.Touch(r, session.Session{ID: sid, Tool: "claude-code"}, "Edit", []string{
		"src/b.go",
		filepath.Join(other, "lib", "x.go"),
		filepath.Join(denied, "secret.go"),
		filepath.Join(outside, "notes.md"),
		filepath.Join(own, "src", "a.go"),
		filepath.Join(other, "gone", "deleted.go"), // a deleted file's directory no longer exists
	}, nil)

	events := hookruntest.Spooled(t, sp)
	if len(events) != 2 {
		t.Fatalf("want one event per collected repository, got %s", hookruntest.Names(events))
	}
	for i, want := range []struct {
		root, files string
	}{{own, "src/a.go,src/b.go"}, {other, "gone/deleted.go,lib/x.go"}} {
		ev, name := events[i], filepath.Base(want.root)
		if ev.Name != semconv.TermaFilesTouchedEvent || hookruntest.Joined(ev.Attrs[semconv.TermaFilesPathsKey]) != want.files {
			t.Errorf("event %d = %+v, want %s's %s", i, ev, name, want.files)
		}
		if ev.Repository.Origin != "github.com/acme/"+name || ev.Attrs[AttrProjectID] != hookruntest.Team {
			t.Errorf("event %d is delivered as %q for %v, want %s's", i, ev.Repository.Origin, ev.Attrs[AttrProjectID], name)
		}
		for k, v := range map[string]string{
			semconv.VCSRepositoryURLFullKey: "https://github.com/acme/" + name, semconv.VCSOwnerNameKey: "acme",
			semconv.VCSRepositoryNameKey: name, semconv.VCSProviderNameKey: "github",
		} {
			if ev.Attrs[k] != v {
				t.Errorf("event %d %s = %v, want %s", i, k, ev.Attrs[k], v)
			}
		}
	}
	if c, ok := claim.Read(stateDir, sid, time.Now()); !ok || c.Repo != filepath.Base(own) || c.Repository.Origin != "github.com/acme/"+filepath.Base(own) {
		t.Errorf("claim = %+v, %v: the session's own repository claims it", c, ok)
	}
	for _, root := range claimed {
		if root != own {
			t.Errorf("OnClaim(%q): only the session's own repository is wired", root)
		}
	}
	for root, want := range map[string]string{own: "src/a.go,src/b.go", other: "", denied: ""} {
		if got := manifestFiles(t, root, sid); got != want {
			t.Errorf("%s's manifest = %q, want %q", filepath.Base(root), got, want)
		}
	}
	if blob := hookruntest.Names(events) + hookruntest.Joined(events[0].Attrs[semconv.TermaFilesPathsKey]) + hookruntest.Joined(events[1].Attrs[semconv.TermaFilesPathsKey]); strings.Contains(blob, "secret") || strings.Contains(blob, "notes") {
		t.Errorf("a file outside the collected repositories was spooled: %s", blob)
	}
}

// In global mode a session in a folder outside git still reports its files, naming no repository.
func TestTouchInAGlobalFolderOutsideGitNamesNoRepository(t *testing.T) {
	t.Parallel()
	dir, _ := filepath.EvalSymlinks(t.TempDir()) // a folder outside git is located with symlinks resolved
	stateDir := t.TempDir()
	sp, _ := spool.Open(t.TempDir())
	env := Env{StateDir: stateDir, Now: time.Now(), Cwd: dir, Spool: sp, Policy: config.Policy{Mode: config.ModeGlobal, DefaultProjectID: "p-default"}}
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	env.Touch(r, session.Session{ID: "sess-touch-3", Tool: "claude-code"}, "Write", []string{"notes.md"}, nil)
	events := hookruntest.Spooled(t, sp)
	if len(events) != 1 || hookruntest.Joined(events[0].Attrs[semconv.TermaFilesPathsKey]) != "notes.md" {
		t.Fatalf("spooled %+v", events)
	}
	for _, k := range []string{semconv.VCSRepositoryURLFullKey, semconv.VCSRepositoryNameKey, semconv.VCSOwnerNameKey} {
		if v, ok := events[0].Attrs[k]; ok {
			t.Errorf("%s = %v outside git", k, v)
		}
	}
}

// Files only in a repository the team does not collect, or outside git, spool nothing.
func TestTouchOutsideCollectedRepositoriesSpoolsNothing(t *testing.T) {
	t.Parallel()
	root, stateDir, sp := hookruntest.Project(t)
	denied := hookruntest.InitRepo(t)
	env := Env{StateDir: stateDir, Now: time.Now(), Cwd: root, Spool: sp, Team: hookruntest.Team, Policy: hookruntest.Admitting(root)}
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	env.Touch(r, session.Session{ID: "sess-touch-2", Tool: "codex"}, "apply_patch", []string{
		filepath.Join(denied, "a.go"), filepath.Join(t.TempDir(), "b.go"), "",
	}, nil)
	if events := hookruntest.Spooled(t, sp); len(events) != 0 {
		t.Fatalf("spooled %s", hookruntest.Names(events))
	}
}

func manifestFiles(t *testing.T, root, sid string) string {
	t.Helper()
	ms, err := hookruntest.Store(t, root).Manifests()
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, m := range ms {
		if m.SessionID == sid {
			for f := range m.Files {
				files = append(files, f)
			}
		}
	}
	slices.Sort(files)
	return strings.Join(files, ",")
}
