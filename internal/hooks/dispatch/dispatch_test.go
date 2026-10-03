package dispatch

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// fake is an agent with one event, one render hook and one hooks-off handler, recording
// what ran.
type fake struct{ ran *[]string }

func (fake) Name() string                   { return "fake" }
func (fake) DisplayName() string            { return "Fake" }
func (fake) Installed(context.Context) bool { return true }
func (fake) FlushAfter() []string           { return []string{"fake-stop"} }
func (f fake) record(name string) agents.Handler {
	return func(context.Context, hookrun.Env) error { *f.ran = append(*f.ran, name); return nil }
}
func (f fake) Events() map[string]agents.Handler {
	return map[string]agents.Handler{"fake-edit": f.record("edit"), "fake-stop": f.record("stop")}
}
func (f fake) Renders() map[string]agents.RenderHandler {
	return map[string]agents.RenderHandler{"fake-line": func(_ context.Context, env hookrun.Env) int {
		*f.ran = append(*f.ran, "render spool="+map[bool]string{true: "on", false: "off"}[env.Spool != nil])
		return 7
	}}
}
func (f fake) WhenHooksOff() map[string]agents.Handler {
	return map[string]agents.Handler{"fake-edit": f.record("off")}
}

type harness struct {
	ran             []string
	flushes, claims int
	deps            Deps
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{}
	s, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.deps = Deps{
		Agents:  agents.New(fake{ran: &h.ran}),
		Profile: func() Profile { return Profile{Policy: config.Policy{Mode: config.ModeRepo}} },
		Spool:   func() *spool.Spool { return s },
		Claimed: func(context.Context, string) { h.claims++ },
		Flush:   func() { h.flushes++ },
	}
	return h
}

func request(event string) Request {
	return Request{Event: event, Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Cwd: "/nowhere"}
}

// A render hook's status is the process's, and it still renders with hooks off, without
// capture.
func TestRunReturnsTheRenderStatus(t *testing.T) {
	h := newHarness(t)
	if got := Run(context.Background(), h.deps, request("fake-line")); got != 7 {
		t.Fatalf("status %d, want the renderer's 7", got)
	}
	off := request("fake-line")
	off.HooksOff = true
	Run(context.Background(), h.deps, off)
	if strings.Join(h.ran, ",") != "render spool=on,render spool=off" {
		t.Fatalf("ran %v", h.ran)
	}
}

// With hooks off only the hooks-off handler runs, and nothing flushes.
func TestRunWithHooksOffRunsOnlyTheOffHandler(t *testing.T) {
	h := newHarness(t)
	r := request("fake-edit")
	r.HooksOff = true
	if got := Run(context.Background(), h.deps, r); got != 0 {
		t.Fatalf("status %d", got)
	}
	if strings.Join(h.ran, ",") != "off" || h.flushes != 0 {
		t.Fatalf("ran %v, %d flushes", h.ran, h.flushes)
	}
}

// An event runs its handler, and a flush follows only the events that ask for one.
func TestRunFlushesAfterTheEventsThatAsk(t *testing.T) {
	h := newHarness(t)
	Run(context.Background(), h.deps, request("fake-edit"))
	if h.flushes != 0 {
		t.Fatal("an edit flushed")
	}
	Run(context.Background(), h.deps, request("fake-stop"))
	if strings.Join(h.ran, ",") != "edit,stop" || h.flushes != 1 {
		t.Fatalf("ran %v, %d flushes", h.ran, h.flushes)
	}
}

// An event no agent declares is ignored with a word on stderr.
func TestRunIgnoresAnUnknownEvent(t *testing.T) {
	h := newHarness(t)
	r := request("nobody-handles-this")
	if got := Run(context.Background(), h.deps, r); got != 0 || !strings.Contains(r.Stderr.(*bytes.Buffer).String(), "unknown event") {
		t.Fatalf("status %d, stderr %q", got, r.Stderr.(*bytes.Buffer).String())
	}
}

// A git hook never reaches the registry: it is the commit's critical path.
func TestRunGitHooksNeedNoRegistry(t *testing.T) {
	h := newHarness(t)
	h.deps.Agents = nil
	r := request("prepare-commit-msg")
	r.Cwd = t.TempDir()
	if got := Run(context.Background(), h.deps, r); got != 0 {
		t.Fatalf("status %d", got)
	}
}

// A hook where the team does not collect marks its session but claims nothing, so it
// starts no relay.
func TestRunMarksWithoutClaiming(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	hookruntest.RelayOn(t)
	h := newHarness(t)
	h.deps.Agents = agents.New(reading{fake{ran: &h.ran}})
	h.deps.Profile = func() Profile {
		return Profile{Team: "t1", Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/work"}, TeamID: "t1", FetchedAt: time.Now()}}
	}
	r := request("fake-edit")
	r.Cwd, _ = filepath.EvalSymlinks(t.TempDir())
	r.Stdin = strings.NewReader(`{"session_id":"s1","cwd":"` + hookruntest.InJSON(r.Cwd) + `"}`)
	Run(context.Background(), h.deps, r)
	if c, ok := claim.Read("s1", time.Now()); !ok || c.ProjectID != "" {
		t.Fatalf("mark = %+v, %v", c, ok)
	}
	if h.claims != 0 {
		t.Fatal("a mark started the relay")
	}
}

// reading is fake whose edit handler reads its payload, as every real handler does.
type reading struct{ fake }

func (reading) Events() map[string]agents.Handler {
	return map[string]agents.Handler{"fake-edit": func(_ context.Context, env hookrun.Env) error {
		_, err := io.ReadAll(env.Stdin)
		return err
	}}
}
