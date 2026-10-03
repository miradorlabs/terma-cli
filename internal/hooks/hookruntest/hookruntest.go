// Package hookruntest is what a hook handler's tests share: a sandboxed repository and
// the events a handler spooled.
package hookruntest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// InitRepo creates a git repository with a private terma config directory and returns its
// resolved path. Git's global and system config are ignored, since a developer's global
// core.hooksPath would run the installed terma's hooks inside the test.
func InitRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "dev@example.com"},
		{"config", "user.name", "Dev"},
		{"config", "commit.gpgsign", "false"},
	} {
		if _, err := gitx.Git(context.Background(), dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	resolved, _ := filepath.EvalSymlinks(dir)
	return resolved
}

// WriteFile writes content to rel under root, creating its directories.
func WriteFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ReadFile is rel under root, or "" when it cannot be read.
func ReadFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	return string(data)
}

// Spooled flushes the spool and returns every event in it, in order.
func Spooled(t *testing.T, sp *spool.Spool) []spool.Event {
	t.Helper()
	var out []spool.Event
	res := sp.Flush(context.Background(), spool.SenderFunc(func(_ context.Context, events []spool.Event) ([]spool.Event, error) {
		out = append(out, events...)
		return nil, nil
	}), spool.FlushOptions{Force: true})
	if res.Err != nil {
		t.Fatalf("flush: %v", res.Err)
	}
	return out
}

// Team is the team the developer chose at setup in these tests.
const Team = "project-a"

// Project is a repository and a private spool for its hooks, which run for Team.
func Project(t *testing.T) (root string, sp *spool.Spool) {
	t.Helper()
	root = InitRepo(t)
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root, sp
}

// Named is the events called name, in order.
func Named(events []spool.Event, name string) []spool.Event {
	var out []spool.Event
	for _, e := range events {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

// Num reads a spooled number back, which JSON decodes as float64 whatever the hook put in.
func Num(v any) float64 {
	f, _ := v.(float64)
	return f
}

// Names is the events' names, space-separated, for a failure message.
func Names(events []spool.Event) string {
	var n []string
	for _, e := range events {
		n = append(n, e.Name)
	}
	return strings.Join(n, " ")
}

// Lifecycle drops the funding and observation events spooled alongside a session's lifecycle.
func Lifecycle(events []spool.Event) []spool.Event {
	var out []spool.Event
	for _, e := range events {
		switch e.Name {
		case "terma.session.account", "terma.session.quota", "terma.session.observation", "terma.session.capture":
			continue
		}
		out = append(out, e)
	}
	return out
}

// Store is the session store of the repository at root.
func Store(t *testing.T, root string) *session.Store {
	t.Helper()
	return session.Open(filepath.Join(root, ".git"))
}

// RelayOn gives this machine a local relay token, so hooks claim sessions for it.
func RelayOn(t *testing.T) {
	t.Helper()
	path, err := claim.TokenPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
}
