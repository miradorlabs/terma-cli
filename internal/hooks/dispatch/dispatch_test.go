package dispatch

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/sendas"
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
	return map[string]agents.Handler{"fake-edit": func(ctx context.Context, env hookrun.Env) error {
		// The reply an agent may expect even from a hook that records nothing.
		_, _ = io.WriteString(env.Stdout, "{}\n")
		return f.record("off")(ctx, env)
	}}
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
		ConfigDir: t.TempDir(),
		StateDir:  t.TempDir(),
		Agents:    agents.New(fake{ran: &h.ran}),
		Profile:   func() Profile { return Profile{Policy: config.Policy{Mode: config.ModeRepo}} },
		Spool:     func() *spool.Spool { return s },
		Claimed:   func(string, config.Policy) { h.claims++ },
		Flush:     func() { h.flushes++ },
	}
	hookruntest.RelayOn(t, h.deps.StateDir)
	return h
}

func request(event string) Request {
	return Request{Event: event, Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Cwd: "/nowhere"}
}

// A render hook's status is the process's, and it still renders with hooks off, without
// capture.
func TestRunReturnsTheRenderStatus(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	h := newHarness(t)
	r := request("fake-edit")
	r.HooksOff = true
	if got := Run(context.Background(), h.deps, r); got != 0 {
		t.Fatalf("status %d", got)
	}
	if strings.Join(h.ran, ",") != "off" || h.flushes != 0 {
		t.Fatalf("ran %v, %d flushes", h.ran, h.flushes)
	}
	if got := r.Stdout.(*bytes.Buffer).String(); got != "{}\n" {
		t.Fatalf("the hooks-off handler's reply %q did not reach the agent", got)
	}
}

// Once teardown has retired the relay's token, an agent still running keeps calling its
// hooks: they act as if switched off, so nothing is spooled, claimed or flushed.
func TestRunOnceTornDownActsAsHooksOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := os.Remove(claim.TokenPath(h.deps.StateDir)); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"fake-edit", "fake-stop", "fake-line"} {
		Run(context.Background(), h.deps, request(event))
	}
	if strings.Join(h.ran, ",") != "off,render spool=off" || h.flushes != 0 || h.claims != 0 {
		t.Fatalf("ran %v, %d flushes, %d claims", h.ran, h.flushes, h.claims)
	}
}

// An event runs its handler, and a flush follows only the events that ask for one.
func TestRunFlushesAfterTheEventsThatAsk(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	h := newHarness(t)
	r := request("nobody-handles-this")
	if got := Run(context.Background(), h.deps, r); got != 0 || !strings.Contains(r.Stderr.(*bytes.Buffer).String(), "unknown event") {
		t.Fatalf("status %d, stderr %q", got, r.Stderr.(*bytes.Buffer).String())
	}
}

// A git hook never reaches the registry: it is the commit's critical path.
func TestRunGitHooksNeedNoRegistry(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	h := newHarness(t)
	h.deps.Agents = agents.New(reading{fake{ran: &h.ran}})
	h.deps.Profile = func() Profile {
		return Profile{Team: "t1", Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/work"}, TeamID: "t1", FetchedAt: time.Now()}}
	}
	r := request("fake-edit")
	r.Cwd, _ = filepath.EvalSymlinks(t.TempDir())
	r.Stdin = strings.NewReader(`{"session_id":"s1","cwd":"` + hookruntest.InJSON(r.Cwd) + `"}`)
	Run(context.Background(), h.deps, r)
	if c, ok := claim.Read(h.deps.StateDir, "s1", time.Now()); !ok || c.ProjectID != "" {
		t.Fatalf("mark = %+v, %v", c, ok)
	}
	if h.claims != 0 || h.flushes != 0 {
		t.Fatalf("a mark started the relay (%d) or a flush (%d)", h.claims, h.flushes)
	}
}

// A hook outside the list under a policy no relay is refreshing starts the flush that
// refreshes it, once even for an event a flush follows, and still starts no relay.
func TestRunRefreshesAStalePolicy(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deps.Agents = agents.New(reading{fake{ran: &h.ran}})
	h.deps.Profile = func() Profile {
		return Profile{Team: "t1", Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/work"}, TeamID: "t1", FetchedAt: time.Now().Add(-time.Hour)}}
	}
	r := request("fake-edit")
	r.Cwd, _ = filepath.EvalSymlinks(t.TempDir())
	r.Stdin = strings.NewReader(`{"session_id":"s1","cwd":"` + hookruntest.InJSON(r.Cwd) + `"}`)
	Run(context.Background(), h.deps, r)
	if h.flushes != 1 || h.claims != 0 {
		t.Fatalf("flushes %d, relay starts %d; want 1, 0", h.flushes, h.claims)
	}
	stop := request("fake-stop")
	stop.Cwd = r.Cwd
	stop.Stdin = strings.NewReader(`{"session_id":"s2","cwd":"` + hookruntest.InJSON(r.Cwd) + `"}`)
	h.deps.Profile = func() Profile {
		return Profile{Team: "t2", Policy: config.Policy{Mode: config.ModeRepo, TeamID: "t2", FetchedAt: time.Now().Add(-time.Hour)}}
	}
	Run(context.Background(), h.deps, stop)
	if h.flushes != 2 {
		t.Fatalf("a flush-after event in a stale repository started %d flushes, want 1", h.flushes-1)
	}
}

// reading is fake whose edit handler reads its payload, as every real handler does.
type reading struct{ fake }

func (reading) Events() map[string]agents.Handler {
	read := func(_ context.Context, env hookrun.Env) error {
		_, err := io.ReadAll(env.Stdin)
		return err
	}
	return map[string]agents.Handler{"fake-edit": read, "fake-stop": read}
}

// A claim hands on the checkout its payload names, not the hook's own working directory,
// which cmd moves to the Windows directory in a UNC checkout.
func TestRunHandsOnTheClaimedCheckout(t *testing.T) {
	t.Parallel()
	root := hookruntest.InitRepo(t)
	h := newHarness(t)
	var claimedIn string
	var policy config.Policy
	h.deps.Claimed = func(cwd string, p config.Policy) { claimedIn, policy = cwd, p }
	h.deps.Agents = agents.New(reading{fake{ran: &h.ran}})
	h.deps.Profile = func() Profile {
		p := hookruntest.Admitting(root)
		p.TeamID, p.FetchedAt = hookruntest.Team, time.Now()
		return Profile{Team: hookruntest.Team, Account: hookruntest.Account, Policy: p}
	}
	r := request("fake-edit")
	r.Cwd = t.TempDir()
	r.Stdin = strings.NewReader(`{"session_id":"s1","cwd":"` + hookruntest.InJSON(root) + `"}`)
	Run(context.Background(), h.deps, r)
	if claimedIn != root {
		t.Fatalf("claimed in %q, want the payload's %q", claimedIn, root)
	}
	// The policy goes with it: what a repository gets installed depends on it.
	if policy.TeamID != hookruntest.Team {
		t.Fatalf("policy = %+v, want the profile's", policy)
	}
}

// appender is an agent whose one event appends straight to the spool, with no account of
// its own, as several agents' hooks do.
type appender struct{}

func (appender) Name() string                   { return "appender" }
func (appender) DisplayName() string            { return "Appender" }
func (appender) Installed(context.Context) bool { return true }
func (appender) FlushAfter() []string           { return nil }
func (appender) Events() map[string]agents.Handler {
	return map[string]agents.Handler{"appender-edit": func(_ context.Context, env hookrun.Env) error {
		return env.Spool.Append(spool.Event{Name: "direct"})
	}}
}

// A hook's spool is bound to its profile's lane, so even an append that names no account
// is a person's, or no person's under a server key; signed out, or with a sign-in that
// can't be read, it is lost and counted.
func TestAHooksSpoolIsBoundToItsProfilesLane(t *testing.T) {
	t.Parallel()
	const person = "aaaaaaaaaaaaaaaa"
	for _, tc := range []struct {
		name    string
		profile Profile
		want    string // the account queued, "no person", or the reason lost
	}{
		{"person", Profile{Account: person}, person},
		{"server key", Profile{ServerKey: true}, "no person"},
		{"signed out", Profile{}, sendas.LostSignedOut},
		{"unreadable", Profile{Account: sendas.AccountUnreadable}, sendas.LostUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s, err := spool.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			d := Deps{ConfigDir: t.TempDir(), StateDir: t.TempDir(), Agents: agents.New(appender{}),
				Profile: func() Profile { return tc.profile }, Spool: func() *spool.Spool { return s },
				Claimed: func(string, config.Policy) {}, Flush: func() {}}
			hookruntest.RelayOn(t, d.StateDir)
			Run(context.Background(), d, request("appender-edit"))
			evs, err := s.Peek(10)
			if err != nil {
				t.Fatal(err)
			}
			l, _ := spool.ReadLosses(dir, time.Now())
			got := ""
			switch {
			case len(evs) == 1 && evs[0].NoPerson:
				got = "no person"
			case len(evs) == 1:
				got = evs[0].Account
			case len(l.Counts) == 1:
				for why, n := range l.Counts {
					if n == 1 {
						got = why
					}
				}
			}
			if got != tc.want {
				t.Fatalf("queued %+v, lost %v; want %s", evs, l.Counts, tc.want)
			}
		})
	}
}

// otherHarness marks the hook processes it starts with OTHER_HOOK, as a harness that also
// runs fake's hook entries does.
type otherHarness struct{}

func (otherHarness) StartedHook(getenv func(string) string) bool { return getenv("OTHER_HOOK") != "" }

// marked reads OTHER_HOOK as set, and nothing else.
func marked(k string) string {
	if k == "OTHER_HOOK" {
		return "1"
	}
	return ""
}

// A hook process another harness started stands down: no handler, claim, flush or reply,
// with hooks on or off. A render hook still renders, and stderr says why only with Debug.
func TestRunStandsDownInAnotherHarness(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deps.Agents = agents.New(reading{fake{ran: &h.ran}}).Starters(otherHarness{})
	root := hookruntest.InitRepo(t)
	h.deps.Profile = func() Profile {
		p := hookruntest.Admitting(root)
		p.TeamID, p.FetchedAt = hookruntest.Team, time.Now()
		return Profile{Team: hookruntest.Team, Account: hookruntest.Account, Policy: p}
	}
	for _, off := range []bool{false, true} {
		for _, event := range []string{"fake-edit", "fake-stop"} {
			r := request(event)
			r.Getenv, r.HooksOff, r.Cwd = marked, off, root
			r.Stdin = strings.NewReader(`{"session_id":"s1","cwd":"` + hookruntest.InJSON(root) + `"}`)
			if got := Run(context.Background(), h.deps, r); got != 0 {
				t.Fatalf("%s: status %d", event, got)
			}
			if out, errs := r.Stdout.(*bytes.Buffer).String(), r.Stderr.(*bytes.Buffer).String(); out != "" || errs != "" {
				t.Errorf("%s (hooks off %v): stdout %q, stderr %q", event, off, out, errs)
			}
		}
	}
	if len(h.ran) != 0 || h.flushes != 0 || h.claims != 0 {
		t.Fatalf("ran %v, %d flushes, %d claims", h.ran, h.flushes, h.claims)
	}
	if _, ok := claim.Read(h.deps.StateDir, "s1", time.Now()); ok {
		t.Fatal("a session another harness started was claimed or marked")
	}
	r := request("fake-edit")
	r.Getenv, r.Debug = marked, true
	Run(context.Background(), h.deps, r)
	if errs := r.Stderr.(*bytes.Buffer).String(); strings.Count(errs, "\n") != 1 || !strings.Contains(errs, "fake-edit stands down") {
		t.Errorf("with Debug, stderr %q", errs)
	}
	if r.Stdout.(*bytes.Buffer).Len() != 0 {
		t.Errorf("with Debug, stdout %q", r.Stdout.(*bytes.Buffer).String())
	}
	line := request("fake-line")
	line.Getenv = marked
	if got := Run(context.Background(), h.deps, line); got != 7 {
		t.Fatalf("render status %d, want 7", got)
	}
	if strings.Join(h.ran, ",") != "render spool=on" {
		t.Fatalf("ran %v", h.ran)
	}
}

// Without the mark the same events run as before, and claim their session.
func TestRunWithoutTheMarkRuns(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deps.Agents = agents.New(reading{fake{ran: &h.ran}}).Starters(otherHarness{})
	root := hookruntest.InitRepo(t)
	h.deps.Profile = func() Profile {
		p := hookruntest.Admitting(root)
		p.TeamID, p.FetchedAt = hookruntest.Team, time.Now()
		return Profile{Team: hookruntest.Team, Account: hookruntest.Account, Policy: p}
	}
	unmarked := func(string) string { return "" }
	for _, event := range []string{"fake-edit", "fake-stop"} {
		r := request(event)
		r.Getenv, r.Cwd = unmarked, root
		r.Stdin = strings.NewReader(`{"session_id":"s1","cwd":"` + hookruntest.InJSON(root) + `"}`)
		Run(context.Background(), h.deps, r)
	}
	if c, ok := claim.Read(h.deps.StateDir, "s1", time.Now()); !ok || c.ProjectID == "" || h.claims != 2 || h.flushes != 1 {
		t.Fatalf("claim %+v, %v; %d claims, %d flushes", c, ok, h.claims, h.flushes)
	}
	h.deps.Agents = agents.New(fake{ran: &h.ran}).Starters(otherHarness{})
	off := request("fake-edit")
	off.Getenv, off.HooksOff = unmarked, true
	Run(context.Background(), h.deps, off)
	if strings.Join(h.ran, ",") != "off" {
		t.Fatalf("ran %v", h.ran)
	}
	if got := off.Stdout.(*bytes.Buffer).String(); got != "{}\n" {
		t.Fatalf("hooks-off reply %q", got)
	}
}

// A git hook runs in a process another harness started too, and still never reaches the
// registry: a commit made from it is still stamped.
func TestRunGitHooksIgnoreTheMark(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deps.Agents = nil
	for _, event := range []string{"prepare-commit-msg", "post-commit", "pre-push"} {
		r := request(event)
		r.Cwd, r.Getenv = t.TempDir(), marked
		if got := Run(context.Background(), h.deps, r); got != 0 {
			t.Fatalf("%s: status %d", event, got)
		}
	}
	// post-commit's flush shows it ran.
	if h.flushes != 1 {
		t.Fatalf("%d flushes, want post-commit's 1", h.flushes)
	}
}
