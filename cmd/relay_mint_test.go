package cmd

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

const mintedKey = "ter_srv_minted0123456789abcdefghijklmnopqrstuv"

// A claimed project with no key on this machine — a repository the platform connected —
// asks for one to be minted, and waits; with a key, the organization's policy is the
// ceiling on content and the developer's record can only narrow it.
func TestRelayResolverMintsAndCapsContent(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	cfg := &config.Config{OTLPURL: "https://otel.example", Policy: config.DefaultPolicy()}
	var asked []string
	resolve := relayResolver(cfg, func(p string) { asked = append(asked, p) })
	c := claim.Claim{ProjectID: "p1", Tool: "codex"}
	if _, err := resolve(c); !errors.Is(err, relay.ErrNoKey) || len(asked) != 1 || asked[0] != "p1" {
		t.Fatalf("keyless: err %v, asked %v", err, asked)
	}
	if err := keystore.Set("p1", mintedKey, keystore.HostsOf(cfg)); err != nil {
		t.Fatal(err)
	}
	pol, err := resolve(c)
	if err != nil || !pol.IncludePrompts || !pol.IncludeToolContent || pol.Key != mintedKey {
		t.Fatalf("no record: %+v, %v (the policy's defaults apply)", pol, err)
	}
	if err := routing.SaveRecord(routing.Record{ProjectID: "p1", IncludePrompts: false, IncludeToolContent: true, Harnesses: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	if pol, _ := resolve(c); pol.IncludePrompts || !pol.IncludeToolContent {
		t.Fatalf("the developer's prompts-off did not narrow: %+v", pol)
	}
	cfg.Policy.IncludeToolContent = false
	if pol, _ := resolve(c); pol.IncludeToolContent {
		t.Fatalf("the organization's tool-content-off was widened by the record: %+v", pol)
	}
}

// The relay mints a missing key once, stores it as the project's, and after a failure
// does not ask again until the backoff passes.
func TestRelayKeyMinter(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	cfg := &config.Config{Policy: config.DefaultPolicy()}
	m := newRelayKeyMinter(t.Context(), cfg)
	var calls atomic.Int32
	fail := atomic.Bool{}
	m.create = func(_ context.Context, _ *config.Config, projectID string) (string, error) {
		calls.Add(1)
		if fail.Load() {
			return "", errors.New("refused")
		}
		return mintedKey, nil
	}
	m.mint("p1")
	m.mint("p1") // in flight or done: one mint
	waitUntil(t, func() bool { return keystore.Get("p1") == mintedKey })
	m.mint("p1")
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("minted %d times for one project", n)
	}

	fail.Store(true)
	m.mint("p2")
	waitUntil(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return !m.failedAt["p2"].IsZero()
	})
	m.mint("p2")
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 2 {
		t.Fatalf("a failed project was asked again inside its backoff: %d calls", n)
	}

	withKey := newRelayKeyMinter(t.Context(), &config.Config{APIKey: "ter_srv_x"})
	withKey.create = m.create
	withKey.mint("p3")
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 2 {
		t.Fatal("a server key tried to mint another")
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
	}
}

// The first claiming hook in a clone of a repository the platform connected points git
// at the committed commit-hook shims; a clone wired before is left alone, also when its
// developer moved core.hooksPath since.
func TestFirstHookWiresTheClone(t *testing.T) {
	repo := installRepo(t)
	if err := termaproject.Save(repo, &termaproject.File{
		Project: termaproject.Project{ID: testProjectID},
		Install: termaproject.Install{HookManager: string(hookmgr.GitShim)},
	}); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	wireCloneOnFirstUse(ctx, repo)
	if got := gitx.ConfigGet(ctx, repo, "core.hooksPath"); got != hookmgr.ShimDir {
		t.Fatalf("core.hooksPath = %q, want %q", got, hookmgr.ShimDir)
	}
	if _, err := gitx.Git(ctx, repo, "config", "--worktree", "core.hooksPath", "elsewhere"); err != nil {
		if _, err := gitx.Git(ctx, repo, "config", "core.hooksPath", "elsewhere"); err != nil {
			t.Fatal(err)
		}
	}
	wireCloneOnFirstUse(ctx, repo)
	if got := gitx.ConfigGet(ctx, repo, "core.hooksPath"); got != "elsewhere" {
		t.Fatalf("a wired clone was rewired: %q", got)
	}
}

// A binding whose commit hooks run through a manager (husky, lefthook) needs nothing
// per clone: the hook leaves git's config alone.
func TestFirstHookLeavesAManagedRepositoryAlone(t *testing.T) {
	repo := installRepo(t)
	if err := termaproject.Save(repo, &termaproject.File{
		Project: termaproject.Project{ID: testProjectID},
		Install: termaproject.Install{HookManager: "husky"},
	}); err != nil {
		t.Fatal(err)
	}
	wireCloneOnFirstUse(t.Context(), repo)
	if got := gitx.ConfigGet(t.Context(), repo, "core.hooksPath"); got != "" {
		t.Fatalf("core.hooksPath = %q in a husky repository", got)
	}
}
