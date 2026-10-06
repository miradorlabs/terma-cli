// Package hookruntest is what a hook handler's tests share: a sandboxed repository and
// the events a handler spooled.
package hookruntest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// InitRepo creates a git repository, origin github.com/acme/<its folder>, and returns its
// resolved path. Its own core.hooksPath, an empty directory, outranks a developer's global
// one, which would run the installed terma's post-commit inside the test and consume its
// manifests; set in the repository, not the environment, so the test can run in parallel.
// It also means terma's installer skips the repository (internal/repohooks): a test about
// what a session installs builds its own.
func InitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "core.hooksPath", t.TempDir()},
		{"config", "user.email", "dev@example.com"},
		{"config", "user.name", "Dev"},
		{"config", "commit.gpgsign", "false"},
		{"remote", "add", "origin", "https://github.com/acme/" + filepath.Base(dir) + ".git"},
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

// spooledAsOf is when Spooled flushes: before every fixture's date, so none has expired
// (spool.MaxAge) however long ago its transcript was recorded.
var spooledAsOf = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// Spooled flushes the spool and returns every event in it, in order.
func Spooled(t *testing.T, sp *spool.Spool) []spool.Event {
	t.Helper()
	var out []spool.Event
	res := sp.Flush(context.Background(), spool.SenderFunc(func(_ context.Context, events []spool.Event) ([]spool.Event, error) {
		out = append(out, events...)
		return nil, nil
	}), spool.FlushOptions{Force: true, Now: spooledAsOf})
	if res.Err != nil {
		t.Fatalf("flush: %v", res.Err)
	}
	return out
}

// Team is the team the developer chose at setup in these tests.
const Team = "project-a"

// Admitting is a repository-mode policy, content on, that lists the repository InitRepo
// made at root.
func Admitting(root string) config.Policy {
	p := config.DefaultPolicy()
	p.Repositories = []string{"github.com/acme/" + filepath.Base(root)}
	return p
}

// Project is a repository, and a private state directory and spool for its hooks, which
// run for Team.
func Project(t *testing.T) (root, stateDir string, sp *spool.Spool) {
	t.Helper()
	root = InitRepo(t)
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root, t.TempDir(), sp
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

// Joined reads a spooled string list back, which JSON decodes as []any, comma-joined.
func Joined(v any) string {
	list, _ := v.([]any)
	parts := make([]string, 0, len(list))
	for _, p := range list {
		parts = append(parts, fmt.Sprint(p))
	}
	return strings.Join(parts, ",")
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
		case semconv.TermaSessionAccountEvent, semconv.TermaSessionQuotaEvent, semconv.TermaSessionObservationEvent, semconv.TermaSessionCaptureEvent:
			continue
		}
		out = append(out, e)
	}
	return out
}

// Store is the session store of the repository at root.
func Store(t *testing.T, root string) *session.Store {
	t.Helper()
	return session.Open(filepath.Join(root, ".git", project.GitStoreDir))
}

// RelayOn gives the state directory stateDir a local relay token, so hooks claim
// sessions for it.
func RelayOn(t *testing.T, stateDir string) {
	t.Helper()
	path := claim.TokenPath(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// InJSON is s as it reads inside a JSON string, the way an agent writes a path into a
// payload: a Windows path's backslashes escaped.
func InJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}
