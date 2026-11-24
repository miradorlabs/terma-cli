package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

const (
	routedAuth = "https://auth.example"
	aliceLane  = "aaaaaaaaaaaaaaaa"
	bobLane    = "bbbbbbbbbbbbbbbb"
)

type routedMachine struct {
	t             *testing.T
	config, state string
	spool         *spool.Spool
}

func newRoutedMachine(t *testing.T) *routedMachine {
	t.Helper()
	m := &routedMachine{t: t, config: t.TempDir(), state: t.TempDir()}
	s, err := spool.Open(filepath.Join(m.state, spool.Dir))
	if err != nil {
		t.Fatal(err)
	}
	m.spool = s
	return m
}

func orgTeam(org, id string, entries ...string) routing.Team {
	return routing.Team{
		OrganizationID: org, AuthURL: routedAuth, TeamID: id,
		Policy: config.Policy{
			Mode: config.ModeRepo, TeamID: id, OrganizationID: org, AuthURL: routedAuth,
			Repositories: entries, Wildcards: true, Revision: 1, FetchedAt: time.Now().Add(-time.Hour),
			IncludePrompts: true, IncludeToolContent: true,
		},
	}
}

func personalTeam() routing.Team {
	t := orgTeam("org-me", "team-personal")
	t.Personal = true
	t.Policy.Mode = config.ModeGlobal
	return t
}

func (m *routedMachine) createState(profile, account string, teams []routing.Team, change func(*routing.State, map[string]string) error) map[string]string {
	m.t.Helper()
	enrolled := map[string]string{}
	_, err := routing.Create(m.state, profile, account, func(s *routing.State) error {
		for _, tm := range teams {
			id, err := s.Select(tm)
			if err != nil {
				return err
			}
			enrolled[tm.TeamID] = id
		}
		if change != nil {
			return change(s, enrolled)
		}
		return nil
	})
	if err != nil {
		m.t.Fatal(err)
	}
	return enrolled
}

func (m *routedMachine) updateState(profile, account string, change func(*routing.State) error) {
	m.t.Helper()
	if _, err := routing.Update(m.state, profile, routing.Expect{Account: account}, change); err != nil {
		m.t.Fatal(err)
	}
}

func (m *routedMachine) queueRaw(events ...spool.Event) {
	m.t.Helper()
	var lines []byte
	for _, e := range events {
		if e.Time.IsZero() {
			e.Time = time.Now()
		}
		line, err := json.Marshal(e)
		if err != nil {
			m.t.Fatal(err)
		}
		lines = append(append(lines, line...), '\n')
	}
	path := filepath.Join(m.state, spool.Dir, spool.EventsFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		m.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(lines); err != nil {
		m.t.Fatal(err)
	}
}

func (m *routedMachine) pending() int {
	n, _, _ := m.spool.Pending()
	return n
}

// router panics if Policy is ever called in routed mode.
func (m *routedMachine) router(signIn SignIn) Router {
	return Router{
		ConfigDir: m.config,
		StateDir:  m.state,
		SignIn:    signIn,
		Policy: func(context.Context, *config.Config, string) (config.Policy, error) {
			m.t.Fatal("routed delivery must never call single-team Policy")
			return config.Policy{}, nil
		},
	}
}

func routedEvent(account, profile, enrollment, repo, name string) spool.Event {
	e := spool.Event{
		Time:       time.Now(),
		Name:       name,
		Repository: config.Repository{Origin: repo},
		Profile:    profile,
		Enrollment: enrollment,
		Attrs:      map[string]any{},
	}
	if account == sendas.NoPersonLane {
		e.NoPerson = true
	} else {
		e.Account = account
	}
	return e
}

// Invariant 1: Wrong-lane events are gated BEFORE reading routing.State, run ledger, or
// policy; they stay held untouched (even when routing.State is corrupt or Enrollment == "")
// and age out after spool.MaxAge (14 days).
func TestRoutedLaneGateRunsBeforeRoutingStateAndPolicy(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t)
	// Write a corrupt routing/default.json so RoutingActive is true and any read would fail.
	statePath, err := routing.StatePath(m.state, config.DefaultProfile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	srv, sent := recordingIngest(t, http.StatusOK)
	var stateReads, ledgerReads atomic.Int32
	r := m.router(&fakeSignIn{account: aliceLane, token: "tok-alice"})
	r.loadState = func(dir, profile string) (routing.State, bool, error) {
		stateReads.Add(1)
		return routing.Load(dir, profile)
	}
	r.readLedger = func(dir, id string) runs.Snapshot {
		ledgerReads.Add(1)
		return runs.Read(dir, id)
	}

	// Queue Bob's event (with enrollment), Bob's destination-less event, a server-key event,
	// and an expired Bob event (older than 14 days).
	bobEv := routedEvent(bobLane, config.DefaultProfile, "enr-bob", "github.com/acme/app", semconv.TermaToolCallEvent)
	bobEv.SessionID = "sess-bob"
	bobNoDest := routedEvent(bobLane, config.DefaultProfile, "", "github.com/acme/app", semconv.TermaToolCallEvent)
	serverEv := routedEvent(sendas.NoPersonLane, config.DefaultProfile, "enr-srv", "github.com/acme/app", semconv.TermaToolCallEvent)
	expiredBob := routedEvent(bobLane, config.DefaultProfile, "enr-bob", "github.com/acme/app", semconv.TermaToolCallEvent)
	expiredBob.Time = time.Now().Add(-spool.MaxAge - time.Minute)
	m.queueRaw(bobEv, bobNoDest, serverEv, expiredBob)

	res := r.Flush(t.Context(), m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile}, true, 0)
	if !res.Routed || res.Sent != 0 || res.Held != 3 || res.Expired != 1 || res.Unrouted != 0 || res.Unroutable != 0 || res.Discarded != 0 || res.Err != nil {
		t.Fatalf("Flush = %+v", res)
	}
	if stateReads.Load() != 0 || ledgerReads.Load() != 0 || len(sent()) != 0 {
		t.Fatalf("wrong-lane events touched state=%d ledger=%d sent=%d", stateReads.Load(), ledgerReads.Load(), len(sent()))
	}
	// No losses recorded for Bob's destination-less event while Alice is active.
	losses, err := spool.ReadLosses(filepath.Join(m.state, spool.Dir), time.Now())
	if err != nil || losses.Counts[LossNoDestination] != 0 {
		t.Fatalf("losses = %+v, err = %v", losses, err)
	}
	if m.pending() != 3 {
		t.Fatalf("pending = %d, want 3 held events untouched", m.pending())
	}

	// Under server-key mode, person events stay held without reading state; signed-out or
	// unreadable sign-in also holds without reading state.
	for _, tc := range []struct {
		name      string
		serverKey bool
		account   string
	}{
		{"signed-out", false, ""},
		{"unreadable", false, sendas.AccountUnreadable},
		{"mode-skew", false, sendas.NoPersonLane},
	} {
		r2 := m.router(&fakeSignIn{account: tc.account, token: "tok"})
		r2.loadState = func(dir, profile string) (routing.State, bool, error) {
			stateReads.Add(1)
			return routing.Load(dir, profile)
		}
		res2 := r2.Flush(t.Context(), m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, ServerKeySignIn: tc.serverKey}, true, 0)
		if res2.Sent != 0 || res2.Held != 3 || res2.Unrouted != 0 || stateReads.Load() != 0 {
			t.Fatalf("%s: res=%+v stateReads=%d", tc.name, res2, stateReads.Load())
		}
	}
}

// Invariant 2: routing.State.Account must match the event's lane (no_person for server key,
// 16-hex account key for user token). Missing, corrupt, or wrong-account routing.State fails
// closed (Unrouted/Held) without falling back to Profile.Team.
func TestRoutedStateAccountPairingFailsClosed(t *testing.T) {
	t.Parallel()

	t.Run("wrong-account user state fails closed", func(t *testing.T) {
		t.Parallel()
		m := newRoutedMachine(t)
		// Routing state belongs to Bob, while Alice is signed in.
		enr := m.createState(config.DefaultProfile, bobLane, []routing.Team{orgTeam("org-a", "team-a", "github.com/acme/app")}, func(s *routing.State, _ map[string]string) error {
			return s.Bind("github.com/acme/app", "org-a", false)
		})
		srv, sent := recordingIngest(t, http.StatusOK)
		ev := routedEvent(aliceLane, config.DefaultProfile, enr["team-a"], "github.com/acme/app", semconv.TermaToolCallEvent)
		ev.Attrs[hookrun.AttrProjectID] = "team-fallback"
		m.queueRaw(ev)

		res := m.router(&fakeSignIn{account: aliceLane, token: "tok-alice"}).Flush(
			t.Context(), m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, Team: "team-fallback"}, true, 0)
		if !res.Routed || res.Sent != 0 || res.Held != 1 || res.Unrouted != 1 || res.Discarded != 0 || len(sent()) != 0 {
			t.Fatalf("wrong-account user state: res=%+v sent=%d", res, len(sent()))
		}
	})

	t.Run("user event against no_person state fails closed", func(t *testing.T) {
		t.Parallel()
		m := newRoutedMachine(t)
		enr := m.createState(config.DefaultProfile, sendas.NoPersonLane, []routing.Team{orgTeam("org-a", "team-a", "github.com/acme/app")}, func(s *routing.State, _ map[string]string) error {
			return s.Bind("github.com/acme/app", "org-a", false)
		})
		srv, sent := recordingIngest(t, http.StatusOK)
		m.queueRaw(routedEvent(aliceLane, config.DefaultProfile, enr["team-a"], "github.com/acme/app", semconv.TermaToolCallEvent))

		res := m.router(&fakeSignIn{account: aliceLane, token: "tok-alice"}).Flush(
			t.Context(), m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile}, true, 0)
		if !res.Routed || res.Sent != 0 || res.Held != 1 || res.Unrouted != 1 || len(sent()) != 0 {
			t.Fatalf("user vs no_person state: res=%+v sent=%d", res, len(sent()))
		}
	})

	t.Run("server-key event against user state fails closed", func(t *testing.T) {
		t.Parallel()
		m := newRoutedMachine(t)
		enr := m.createState(config.DefaultProfile, aliceLane, []routing.Team{orgTeam("org-a", "team-a", "github.com/acme/app")}, func(s *routing.State, _ map[string]string) error {
			return s.Bind("github.com/acme/app", "org-a", false)
		})
		if err := keystore.Set(m.config, "team-a", testKey, keystore.Hosts{}); err != nil {
			t.Fatal(err)
		}
		srv, sent := recordingIngest(t, http.StatusOK)
		m.queueRaw(routedEvent(sendas.NoPersonLane, config.DefaultProfile, enr["team-a"], "github.com/acme/app", semconv.TermaToolCallEvent))

		res := m.router(nil).Flush(
			t.Context(), m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, ServerKeySignIn: true}, true, 0)
		if !res.Routed || res.Sent != 0 || res.Held != 1 || res.Unrouted != 1 || len(sent()) != 0 {
			t.Fatalf("server-key vs user state: res=%+v sent=%d", res, len(sent()))
		}
	})

	t.Run("missing state for event profile fails closed", func(t *testing.T) {
		t.Parallel()
		m := newRoutedMachine(t)
		// Another profile has a routing state (routing active), while "missing" does not.
		m.createState("other", aliceLane, []routing.Team{personalTeam()}, nil)
		srv, sent := recordingIngest(t, http.StatusOK)
		ev := routedEvent(aliceLane, "missing", "enr-1", "github.com/acme/app", semconv.TermaToolCallEvent)
		ev.Attrs[hookrun.AttrProjectID] = "team-fallback"
		m.queueRaw(ev)

		res := m.router(&fakeSignIn{account: aliceLane, token: "tok-alice"}).Flush(
			t.Context(), m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: "missing", Team: "team-fallback"}, true, 0)
		if !res.Routed || res.Sent != 0 || res.Held != 1 || res.Unrouted != 1 || len(sent()) != 0 {
			t.Fatalf("missing profile state: res=%+v sent=%d", res, len(sent()))
		}
	})

	t.Run("missing all routing states still fails closed for routed event", func(t *testing.T) {
		t.Parallel()
		m := newRoutedMachine(t)
		srv, sent := recordingIngest(t, http.StatusOK)
		ev := routedEvent(aliceLane, config.DefaultProfile, "enr-deleted", "github.com/acme/app", semconv.TermaToolCallEvent)
		ev.Attrs[hookrun.AttrProjectID] = "team-fallback"
		m.queueRaw(ev)

		res := m.router(&fakeSignIn{account: aliceLane, token: "tok-alice"}).Flush(
			t.Context(), m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, Team: "team-fallback"}, true, 0)
		if res.Sent != 0 || res.Held != 1 || res.Unrouted != 1 || res.Unroutable != 0 || len(sent()) != 0 {
			t.Fatalf("deleted state for routed event: res=%+v sent=%d", res, len(sent()))
		}
	})

	t.Run("corrupt state fails closed", func(t *testing.T) {
		t.Parallel()
		m := newRoutedMachine(t)
		statePath, err := routing.StatePath(m.state, config.DefaultProfile)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statePath, []byte(`{"profile":"default","account":"bad"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		srv, sent := recordingIngest(t, http.StatusOK)
		ev := routedEvent(aliceLane, config.DefaultProfile, "enr-1", "github.com/acme/app", semconv.TermaToolCallEvent)
		ev.Attrs[hookrun.AttrProjectID] = "team-fallback"
		m.queueRaw(ev)

		res := m.router(&fakeSignIn{account: aliceLane, token: "tok-alice"}).Flush(
			t.Context(), m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, Team: "team-fallback"}, true, 0)
		if !res.Routed || res.Sent != 0 || res.Held != 1 || res.Unrouted != 1 || len(sent()) != 0 {
			t.Fatalf("corrupt state: res=%+v sent=%d", res, len(sent()))
		}
	})
}

// Invariant 3: Once routing is active, events with Enrollment == "" (including pre-activation
// events carrying attrs.project_id) or Profile == "" are dropped and counted (Unroutable +
// spool/losses no_destination), never sent to Profile.Team or re-routed.
func TestRoutedDestinationLessEventsDroppedAndCounted(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t)
	m.createState(config.DefaultProfile, aliceLane, []routing.Team{personalTeam()}, nil)
	srv, sent := recordingIngest(t, http.StatusOK)

	// 1. Pre-activation event with project_id but no Enrollment.
	preAct := spool.Event{
		Time: time.Now(), Name: semconv.TermaCommitEvent, Repository: config.Repository{Origin: "github.com/acme/app"},
		Account: aliceLane, Attrs: map[string]any{hookrun.AttrProjectID: "team-legacy"},
	}
	// 2. Parked sandbox event with Profile set but Enrollment == "".
	parked := routedEvent(aliceLane, config.DefaultProfile, "", "github.com/acme/app", semconv.TermaCommitEvent)
	// 3. Malformed event with Enrollment set but Profile == "".
	noProfile := routedEvent(aliceLane, "", "enr-orphan", "github.com/acme/app", semconv.TermaCommitEvent)
	m.queueRaw(preAct, parked, noProfile)

	res := m.router(&fakeSignIn{account: aliceLane, token: "tok-alice"}).Flush(
		t.Context(), m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, Team: "team-legacy"}, true, 0)
	if !res.Routed || res.Sent != 0 || res.Unroutable != 3 || !res.Lost() || res.Held != 0 || len(sent()) != 0 {
		t.Fatalf("Flush = %+v, sent = %d", res, len(sent()))
	}
	if m.pending() != 0 {
		t.Fatalf("pending = %d, want 0", m.pending())
	}
	losses, err := spool.ReadLosses(filepath.Join(m.state, spool.Dir), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if losses.Counts[LossNoDestination] != 3 {
		t.Fatalf("no_destination losses = %d, want 3 (all: %v)", losses.Counts[LossNoDestination], losses.Counts)
	}
}

// Invariant 4a: Pinned enrollments are rechecked via Router.Recheck (never re-routed),
// HoldKey events wait while pending, send only once bound to the holding org, and discard
// if bound elsewhere or refused; content policy withholds disallowed attributes/events.
func TestRoutedRecheckNeverReroutesAndEnforcesHoldKey(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t)
	enr := m.createState(config.DefaultProfile, aliceLane, []routing.Team{
		orgTeam("org-a", "team-a", "github.com/acme/*"),
		orgTeam("org-b", "team-b", "github.com/beta/*"),
		personalTeam(),
	}, func(s *routing.State, _ map[string]string) error {
		if err := s.Bind("github.com/acme", "org-a", false); err != nil {
			return err
		}
		// Leave github.com/beta/pending unbound (Pending).
		return nil
	})

	srv, sent := recordingIngest(t, http.StatusOK)
	signIn := &fakeSignIn{account: aliceLane, token: "tok-alice"}
	r := m.router(signIn)
	cfg := &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, AuthURL: routedAuth}

	// Queue:
	// - active event for team-a (delivers)
	// - pending-binding event for team-b with HoldKey (blocked/held)
	// - event for an ended enrollment (discarded, NOT re-routed to personal!)
	// - event for team-a outside git (no longer covered -> discarded, NOT re-routed to personal!)
	activeEv := routedEvent(aliceLane, config.DefaultProfile, enr["team-a"], "github.com/acme/api", semconv.TermaToolCallEvent)
	pendingEv := routedEvent(aliceLane, config.DefaultProfile, enr["team-b"], "github.com/beta/repo", semconv.TermaToolCallEvent)
	pendingEv.HoldKey = "github.com/beta"
	endedEv := routedEvent(aliceLane, config.DefaultProfile, "enr-deselected", "github.com/acme/api", semconv.TermaToolCallEvent)
	outsideEv := routedEvent(aliceLane, config.DefaultProfile, enr["team-a"], "", semconv.TermaToolCallEvent)
	m.queueRaw(activeEv, pendingEv, endedEv, outsideEv)

	res := r.Flush(t.Context(), m.spool, cfg, true, 0)
	if res.Sent != 1 || res.Blocked != 1 || res.Held != 1 || res.Discarded != 2 || res.Lost() {
		t.Fatalf("pass 1 = %+v", res)
	}
	if got := sent(); len(got) != 1 || got[0].project != "team-a" {
		t.Fatalf("pass 1 sent = %+v, want only team-a", got)
	}

	// Bind github.com/beta to org-b -> held pending event now delivers to team-b!
	m.updateState(config.DefaultProfile, aliceLane, func(s *routing.State) error {
		return s.Bind("github.com/beta", "org-b", false)
	})
	res2 := r.Flush(t.Context(), m.spool, cfg, true, 0)
	if res2.Sent != 1 || res2.Held != 0 || res2.Blocked != 0 || res2.Discarded != 0 {
		t.Fatalf("pass 2 after bind = %+v", res2)
	}
	if got := sent(); len(got) != 2 || got[1].project != "team-b" {
		t.Fatalf("pass 2 sent = %+v, want team-b", got)
	}

	// HoldKey event when binding is refused -> discarded (never re-routed to personal!).
	m.updateState(config.DefaultProfile, aliceLane, func(s *routing.State) error {
		return s.Refuse("github.com/beta/refused")
	})
	refusedHold := routedEvent(aliceLane, config.DefaultProfile, enr["team-b"], "github.com/beta/refused", semconv.TermaToolCallEvent)
	refusedHold.HoldKey = "github.com/beta/refused"
	m.queueRaw(refusedHold)
	res3 := r.Flush(t.Context(), m.spool, cfg, true, 0)
	if res3.Sent != 0 || res3.Discarded != 1 || res3.Held != 0 {
		t.Fatalf("pass 3 refused binding = %+v", res3)
	}

	// Refused team discards (never re-routes to personal); Suspended team blocks (waits).
	m.updateState(config.DefaultProfile, aliceLane, func(s *routing.State) error {
		_, err := s.ApplyRefresh(routing.Refresh{EnrollmentID: enr["team-a"], Epoch: 0, Failure: routing.OrgAccessLost})
		if err != nil {
			return err
		}
		_, err = s.ApplyRefresh(routing.Refresh{EnrollmentID: enr["team-b"], Epoch: 0, Failure: routing.Terminal})
		return err
	})
	m.queueRaw(
		routedEvent(aliceLane, config.DefaultProfile, enr["team-a"], "github.com/acme/api", semconv.TermaToolCallEvent),
		routedEvent(aliceLane, config.DefaultProfile, enr["team-b"], "github.com/beta/repo", semconv.TermaToolCallEvent),
	)
	res4 := r.Flush(t.Context(), m.spool, cfg, true, 0)
	if res4.Sent != 0 || res4.Discarded != 1 || res4.Blocked != 1 || res4.Held != 1 {
		t.Fatalf("pass 4 refused+suspended = %+v", res4)
	}

	// Content policy withholding: prompts/tool_content off withholds tool results whole & strips args.
	m.updateState(config.DefaultProfile, aliceLane, func(s *routing.State) error {
		pol := s.Teams[len(s.Teams)-1].Policy
		pol.Revision = 2
		pol.IncludePrompts, pol.IncludeToolContent = false, false
		_, err := s.ApplyRefresh(routing.Refresh{EnrollmentID: enr["team-personal"], Epoch: 0, Policy: &pol})
		return err
	})
	toolResultEv := routedEvent(aliceLane, config.DefaultProfile, enr["team-personal"], "", semconv.TermaToolResultEvent)
	toolEv := routedEvent(aliceLane, config.DefaultProfile, enr["team-personal"], "", semconv.TermaToolCallEvent)
	toolEv.Attrs[semconv.GenAIToolCallArgumentsKey] = "secret-args"
	toolEv.Attrs[semconv.UserEmailKey] = "alice@example.com"
	m.queueRaw(toolResultEv, toolEv)
	res5 := r.Flush(t.Context(), m.spool, cfg, true, 0)
	if res5.Sent != 1 || res5.Withheld != 1 {
		t.Fatalf("pass 5 content policy = %+v", res5)
	}
	lastBody := sent()[len(sent())-1].body
	if strings.Contains(lastBody, "secret-args") || strings.Contains(lastBody, "alice@example.com") {
		t.Fatalf("withheld attributes leaked: %s", lastBody)
	}
}

// Invariant 4b (§6 item 9): Content/conversation events drop from MixedAt onward (even with
// OwnLocation: true), whereas non-content locational events (OwnLocation: true &&
// runs.Locational(name)) still leave to their own repo's team after the run turns Mixed.
func TestRoutedMixedDropsContentButKeepsOwnLocationLocational(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t)
	enr := m.createState(config.DefaultProfile, aliceLane, []routing.Team{
		orgTeam("org-a", "team-a", "github.com/acme/app"),
		orgTeam("org-b", "team-b", "github.com/beta/lib"),
	}, func(s *routing.State, _ map[string]string) error {
		if err := s.Bind("github.com/acme/app", "org-a", false); err != nil {
			return err
		}
		return s.Bind("github.com/beta/lib", "org-b", false)
	})

	t0 := time.Now().Add(-10 * time.Minute)
	mixedAt := t0.Add(2 * time.Minute)
	newRunAt := t0.Add(6 * time.Minute)

	if _, err := runs.Start(m.state, "sess-1", t0); err != nil {
		t.Fatal(err)
	}
	if err := runs.MarkMixed(m.state, "sess-1", mixedAt, runs.Why{Reason: runs.Crossed, Where: "github.com/beta/lib"}); err != nil {
		t.Fatal(err)
	}
	if err := runs.Alias(m.state, "sess-1", "subagent-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := runs.Start(m.state, "sess-1", newRunAt); err != nil {
		t.Fatal(err)
	}

	evAt := func(sess, enrollment, repo, name string, at time.Time, ownLoc bool) spool.Event {
		e := routedEvent(aliceLane, config.DefaultProfile, enrollment, repo, name)
		e.SessionID, e.Time, e.OwnLocation = sess, at, ownLoc
		return e
	}

	// 1. Before MixedAt -> leaves.
	before := evAt("sess-1", enr["team-a"], "github.com/acme/app", semconv.TermaToolCallEvent, mixedAt.Add(-time.Second), false)
	// 2. At MixedAt -> drops.
	atMixed := evAt("sess-1", enr["team-a"], "github.com/acme/app", semconv.TermaToolCallEvent, mixedAt, false)
	// 3. Content event after MixedAt even with OwnLocation=true -> drops!
	replyAfter := evAt("sess-1", enr["team-b"], "github.com/beta/lib", semconv.TermaAssistantMessageEvent, mixedAt.Add(time.Minute), true)
	// 4. Subagent alias after MixedAt -> drops!
	subAfter := evAt("subagent-1", enr["team-a"], "github.com/acme/app", semconv.TermaToolCallEvent, mixedAt.Add(time.Minute), false)
	// 5. Locational event WITHOUT OwnLocation after MixedAt -> drops!
	commitNotOwn := evAt("sess-1", enr["team-a"], "github.com/acme/app", semconv.TermaCommitEvent, mixedAt.Add(time.Minute), false)
	// 6. Locational events WITH OwnLocation=true after MixedAt -> STILL LEAVE to their own repo's team!
	commitOwn := evAt("sess-1", enr["team-b"], "github.com/beta/lib", semconv.TermaCommitEvent, mixedAt.Add(time.Minute), true)
	pushOwn := evAt("sess-1", enr["team-b"], "github.com/beta/lib", semconv.TermaPushEvent, mixedAt.Add(time.Minute), true)
	touchOwn := evAt("sess-1", enr["team-a"], "github.com/acme/app", semconv.TermaFilesTouchedEvent, mixedAt.Add(time.Minute), true)
	// 7. New clean run started at newRunAt -> leaves!
	cleanRun := evAt("sess-1", enr["team-a"], "github.com/acme/app", semconv.TermaToolCallEvent, newRunAt.Add(time.Second), false)

	m.queueRaw(before, atMixed, replyAfter, subAfter, commitNotOwn, commitOwn, pushOwn, touchOwn, cleanRun)

	srv, sent := recordingIngest(t, http.StatusOK)
	r := m.router(&fakeSignIn{account: aliceLane, token: "tok-alice"})
	r.Consent = func(string, hookrun.Consent) bool { return true }

	res := r.Flush(t.Context(), m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, AuthURL: routedAuth}, true, 0)
	if res.Sent != 5 || res.Mixed != 4 || res.MixedBy[runs.Crossed] != 4 || res.Lost() {
		t.Fatalf("Mixed flush = %+v", res)
	}
	var bodies []string
	for _, req := range sent() {
		bodies = append(bodies, req.project+":"+req.body)
	}
	joined := strings.Join(bodies, "\n")
	for _, want := range []string{semconv.TermaCommitEvent, semconv.TermaPushEvent, semconv.TermaFilesTouchedEvent} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected locational event %s to leave after Mixed", want)
		}
	}
	if strings.Contains(joined, semconv.TermaAssistantMessageEvent) || strings.Contains(joined, "subagent-1") {
		t.Fatalf("Mixed content leaked: %s", joined)
	}

	// Unreadable ledger fails closed for content events (MixedBy[""]++) while keeping OwnLocation locational.
	badLedgerPath := filepath.Join(m.state, runs.Dir, "sess-corrupt.json")
	if err := os.WriteFile(badLedgerPath, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.queueRaw(
		evAt("sess-corrupt", enr["team-a"], "github.com/acme/app", semconv.TermaToolCallEvent, time.Now(), false),
		evAt("sess-corrupt", enr["team-a"], "github.com/acme/app", semconv.TermaCommitEvent, time.Now(), true),
	)
	res2 := r.Flush(t.Context(), m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, AuthURL: routedAuth}, true, 0)
	if res2.Sent != 1 || res2.Mixed != 1 || res2.MixedBy[""] != 1 {
		t.Fatalf("unreadable ledger = %+v", res2)
	}
}

// Invariant 5: User-token enrollments send with SignIn.Token + X-Mirador-Project + user id;
// server-key (no_person) enrollments send with keys.json and NO X-Mirador-Project header.
func TestRoutedAuthHeadersUserTokenVsServerKey(t *testing.T) {
	t.Parallel()

	t.Run("user token sends X-Mirador-Project and EnrolledUser", func(t *testing.T) {
		t.Parallel()
		m := newRoutedMachine(t)
		enr := m.createState(config.DefaultProfile, aliceLane, []routing.Team{
			orgTeam("org-a", "team-a", "github.com/acme/app"),
			personalTeam(),
		}, func(s *routing.State, _ map[string]string) error {
			return s.Bind("github.com/acme/app", "org-a", false)
		})
		srv, sent := recordingIngest(t, http.StatusOK)
		signIn := &fakeSignIn{account: aliceLane, token: "user-tok-1"}
		r := m.router(signIn)
		var userCalls []string
		r.EnrolledUser = func(profile string, team routing.Team, _ string, account string) string {
			userCalls = append(userCalls, profile+"/"+team.TeamID+"/"+account)
			return "user-alice"
		}
		m.queueRaw(
			routedEvent(aliceLane, config.DefaultProfile, enr["team-a"], "github.com/acme/app", semconv.TermaToolCallEvent),
			routedEvent(aliceLane, config.DefaultProfile, enr["team-personal"], "", semconv.TermaToolCallEvent),
		)

		res := r.Flush(t.Context(), m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, AuthURL: routedAuth}, true, 0)
		if res.Sent != 2 || res.Err != nil {
			t.Fatalf("user-token Flush = %+v", res)
		}
		reqs := sent()
		if len(reqs) != 2 {
			t.Fatalf("sent %d requests, want 2", len(reqs))
		}
		var projects []string
		for _, req := range reqs {
			if req.auth != "Bearer user-tok-1" {
				t.Errorf("auth = %q, want Bearer user-tok-1", req.auth)
			}
			if !strings.Contains(req.body, `"stringValue":"user-alice"`) {
				t.Errorf("missing terma.user.id in %s", req.body)
			}
			projects = append(projects, req.project)
		}
		slices.Sort(projects)
		if !slices.Equal(projects, []string{"team-a", "team-personal"}) {
			t.Fatalf("X-Mirador-Project headers = %v", projects)
		}
		if len(userCalls) != 2 {
			t.Fatalf("EnrolledUser calls = %v", userCalls)
		}

		// Cross-environment guard: cfg.AuthURL mismatch holds without sending.
		m.queueRaw(routedEvent(aliceLane, config.DefaultProfile, enr["team-a"], "github.com/acme/app", semconv.TermaToolCallEvent))
		resOtherEnv := r.Flush(t.Context(), m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, AuthURL: "https://other-env.example"}, true, 0)
		if resOtherEnv.Sent != 0 || resOtherEnv.Held != 1 || len(sent()) != 2 {
			t.Fatalf("cross-env should hold without sending: res=%+v sent=%d", resOtherEnv, len(sent()))
		}
	})

	t.Run("server key sends keys.json key and NO X-Mirador-Project", func(t *testing.T) {
		t.Parallel()
		m := newRoutedMachine(t)
		enr := m.createState(config.DefaultProfile, sendas.NoPersonLane, []routing.Team{
			orgTeam("org-a", "team-srv", "github.com/acme/app"),
		}, func(s *routing.State, _ map[string]string) error {
			return s.Bind("github.com/acme/app", "org-a", false)
		})
		keyHost, keySent := recordingIngest(t, http.StatusOK)
		if err := keystore.Set(m.config, "team-srv", testKey, keystore.Hosts{OTLP: keyHost.URL}); err != nil {
			t.Fatal(err)
		}
		r := m.router(nil)
		r.EnrolledUser = func(string, routing.Team, string, string) string {
			t.Error("server-key delivery must never ask for EnrolledUser")
			return "should-not-appear"
		}
		m.queueRaw(routedEvent(sendas.NoPersonLane, config.DefaultProfile, enr["team-srv"], "github.com/acme/app", semconv.TermaToolCallEvent))

		res := r.Flush(t.Context(), m.spool, &config.Config{
			OTLPURL: "http://127.0.0.1:1", ProfileName: config.DefaultProfile, AuthURL: routedAuth, ServerKeySignIn: true, ServerKeyAuthURL: routedAuth,
		}, true, 0)
		if res.Sent != 1 || res.Err != nil {
			t.Fatalf("server-key Flush = %+v", res)
		}
		reqs := keySent()
		if len(reqs) != 1 {
			t.Fatalf("sent %d requests, want 1", len(reqs))
		}
		if reqs[0].auth != "Bearer "+testKey {
			t.Errorf("auth = %q, want server key", reqs[0].auth)
		}
		if reqs[0].project != "" {
			t.Errorf("server-key delivery must omit X-Mirador-Project, got %q", reqs[0].project)
		}
		if strings.Contains(reqs[0].body, semconv.TermaUserIDKey) {
			t.Errorf("server-key delivery must not stamp terma.user.id: %s", reqs[0].body)
		}

		// Server-key cross-environment guard: mismatched ServerKeyAuthURL or cfg.AuthURL holds.
		m.queueRaw(routedEvent(sendas.NoPersonLane, config.DefaultProfile, enr["team-srv"], "github.com/acme/app", semconv.TermaToolCallEvent))
		resWrongSrvAuth := r.Flush(t.Context(), m.spool, &config.Config{
			OTLPURL: keyHost.URL, ProfileName: config.DefaultProfile, AuthURL: routedAuth, ServerKeySignIn: true, ServerKeyAuthURL: "https://other-env.example",
		}, true, 0)
		if resWrongSrvAuth.Sent != 0 || resWrongSrvAuth.Held != 1 || len(keySent()) != 1 {
			t.Fatalf("mismatched ServerKeyAuthURL should hold: res=%+v sent=%d", resWrongSrvAuth, len(keySent()))
		}
		resWrongEnv := r.Flush(t.Context(), m.spool, &config.Config{
			OTLPURL: keyHost.URL, ProfileName: config.DefaultProfile, AuthURL: "https://other-env.example", ServerKeySignIn: true, ServerKeyAuthURL: "https://other-env.example",
		}, true, 0)
		if resWrongEnv.Sent != 0 || resWrongEnv.Held != 1 || len(keySent()) != 1 {
			t.Fatalf("mismatched team.AuthURL should hold: res=%+v sent=%d", resWrongEnv, len(keySent()))
		}
	})
}

// Invariant 6: Ingest 403 ORG_NOT_ACCESSIBLE backs off only the failing enrollment without
// calling SignIn.Refused or SignIn.Retired; sibling enrollments continue delivering with
// the same user token, and once policy refresh marks the enrollment Refused, queued events
// are discarded via Recheck.
func TestRoutedIngest403OrgNotAccessibleBacksOffOnlyFailingEnrollment(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t)
	enr := m.createState(config.DefaultProfile, aliceLane, []routing.Team{
		orgTeam("org-a", "team-a", "github.com/acme/app"),
		orgTeam("org-b", "team-b", "github.com/beta/app"),
	}, func(s *routing.State, _ map[string]string) error {
		if err := s.Bind("github.com/acme/app", "org-a", false); err != nil {
			return err
		}
		return s.Bind("github.com/beta/app", "org-b", false)
	})

	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		team := r.Header.Get(sendas.ProjectHeader)
		mu.Lock()
		hits[team]++
		mu.Unlock()
		if team == "team-a" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"code":7,"message":"no access","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"ORG_NOT_ACCESSIBLE"}]}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	signIn := &fakeSignIn{account: aliceLane, token: "shared-tok"}
	r := m.router(signIn)
	cfg := &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, AuthURL: routedAuth}

	// Pass 1: team-a returns 403 ORG_NOT_ACCESSIBLE, team-b succeeds on the same pass.
	m.queueRaw(
		routedEvent(aliceLane, config.DefaultProfile, enr["team-a"], "github.com/acme/app", semconv.TermaToolCallEvent),
		routedEvent(aliceLane, config.DefaultProfile, enr["team-b"], "github.com/beta/app", semconv.TermaToolCallEvent),
	)
	res1 := r.Flush(t.Context(), m.spool, cfg, false, 0)
	if res1.Sent != 1 || res1.Failed != 1 || len(res1.Failures) != 1 || res1.Failures[0].ProjectID != "team-a" || res1.Failures[0].RetryAt.IsZero() {
		t.Fatalf("pass 1 = %+v", res1)
	}
	if len(signIn.refused) != 0 || signIn.retired {
		t.Fatalf("403 ORG_NOT_ACCESSIBLE must not rotate or retire user token: refused=%v retired=%v", signIn.refused, signIn.retired)
	}
	if !m.spool.NextAttempt().IsZero() {
		t.Fatalf("per-enrollment 403 must not open spool-wide backoff: %s", m.spool.NextAttempt())
	}
	if windows := m.spool.RetryWindows(time.Now()); len(windows) != 1 || windows[retryDest(enr["team-a"])].IsZero() {
		t.Fatalf("retry windows = %v, want only enrollment/%s", windows, enr["team-a"])
	}

	// Pass 2 (non-forced): team-a waits out its backoff without hitting ingest; team-b delivers.
	m.queueRaw(routedEvent(aliceLane, config.DefaultProfile, enr["team-b"], "github.com/beta/app", semconv.TermaToolCallEvent))
	res2 := r.Flush(t.Context(), m.spool, cfg, false, 0)
	if res2.Sent != 1 || res2.Failed != 1 || res2.Err != nil || len(res2.Waiting) != 1 || res2.Waiting[0].ProjectID != "team-a" {
		t.Fatalf("pass 2 = %+v", res2)
	}
	mu.Lock()
	aHits, bHits := hits["team-a"], hits["team-b"]
	mu.Unlock()
	if aHits != 1 || bHits != 2 {
		t.Fatalf("hits = team-a:%d team-b:%d, want 1 and 2", aHits, bHits)
	}

	// Pass 3: once policy refresh marks org-a Refused, queued team-a event discards via Recheck.
	m.updateState(config.DefaultProfile, aliceLane, func(s *routing.State) error {
		s.ApplyOrgAccessLost("org-a", routedAuth, map[string]int64{enr["team-a"]: 0}, time.Now())
		return nil
	})
	res3 := r.Flush(t.Context(), m.spool, cfg, false, 0)
	if res3.Sent != 0 || res3.Discarded != 1 || res3.Failed != 0 || m.pending() != 0 {
		t.Fatalf("pass 3 after Refused = %+v, pending=%d", res3, m.pending())
	}
}

// 401 still rotates the user token (Refused), and SIGN_IN_RETIRED still retires it (Retired).
func TestRouted401RotatesAndSignInRetiredRetires(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		wantRefused bool
		wantRetired bool
	}{
		{"401 rotates", http.StatusUnauthorized, "", true, false},
		{"403 SIGN_IN_RETIRED retires", http.StatusForbidden, `{"code":7,"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SIGN_IN_RETIRED"}]}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newRoutedMachine(t)
			enr := m.createState(config.DefaultProfile, aliceLane, []routing.Team{personalTeam()}, nil)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)
			signIn := &fakeSignIn{account: aliceLane, token: "tok-1"}
			m.queueRaw(routedEvent(aliceLane, config.DefaultProfile, enr["team-personal"], "", semconv.TermaToolCallEvent))
			res := m.router(signIn).Flush(t.Context(), m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, AuthURL: routedAuth}, true, 0)
			if res.Sent != 0 || len(res.Failures) != 1 {
				t.Fatalf("Flush = %+v", res)
			}
			if gotRefused := len(signIn.refused) == 1; gotRefused != tc.wantRefused || signIn.retired != tc.wantRetired {
				t.Fatalf("refused=%v retired=%v, want %v %v", signIn.refused, signIn.retired, tc.wantRefused, tc.wantRetired)
			}
		})
	}
}

// Pre-POST revalidation: if routing.State changes during an earlier parcel's HTTP request
// (enrollment deselected or content policy tightened), subsequent parcels in the same
// batch do NOT send under the stale admission or stale content policy.
func TestRoutedRevalidatesBeforePostWhenRoutingChangesMidPass(t *testing.T) {
	t.Parallel()

	t.Run("deselected mid-pass discards instead of posting", func(t *testing.T) {
		t.Parallel()
		m := newRoutedMachine(t)
		enr := m.createState(config.DefaultProfile, aliceLane, []routing.Team{
			orgTeam("org-a", "team-a", "github.com/acme/app"),
			orgTeam("org-b", "team-b", "github.com/beta/app"),
		}, func(s *routing.State, _ map[string]string) error {
			if err := s.Bind("github.com/acme/app", "org-a", false); err != nil {
				return err
			}
			return s.Bind("github.com/beta/app", "org-b", false)
		})
		// Determine deterministic parcel order (enrollment IDs are sorted lexicographically).
		secondEnr := enr["team-b"]
		firstTeam, secondTeam := "team-a", "team-b"
		if enr["team-b"] < enr["team-a"] {
			secondEnr = enr["team-a"]
			firstTeam, secondTeam = secondTeam, firstTeam
		}

		var mu sync.Mutex
		var posted []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			team := r.Header.Get(sendas.ProjectHeader)
			mu.Lock()
			posted = append(posted, team)
			mu.Unlock()
			if team == firstTeam {
				m.updateState(config.DefaultProfile, aliceLane, func(s *routing.State) error {
					if !s.Deselect(secondEnr) {
						return errors.New("deselect failed")
					}
					return nil
				})
			}
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)

		m.queueRaw(
			routedEvent(aliceLane, config.DefaultProfile, enr["team-a"], "github.com/acme/app", semconv.TermaToolCallEvent),
			routedEvent(aliceLane, config.DefaultProfile, enr["team-b"], "github.com/beta/app", semconv.TermaToolCallEvent),
		)
		r := m.router(&fakeSignIn{account: aliceLane, token: "tok"})
		r.maxConcurrency = 1
		res := r.Flush(
			t.Context(), m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, AuthURL: routedAuth}, true, 0,
		)
		if res.Sent != 1 || res.Discarded != 1 || res.Err != nil {
			t.Fatalf("Flush = %+v, want Sent=1 Discarded=1", res)
		}
		mu.Lock()
		defer mu.Unlock()
		if !slices.Equal(posted, []string{firstTeam}) {
			t.Fatalf("posted = %v, want only [%s] (deselected %s must not POST)", posted, firstTeam, secondTeam)
		}
	})

	t.Run("content policy tightened mid-pass strips content before posting", func(t *testing.T) {
		t.Parallel()
		m := newRoutedMachine(t)
		enr := m.createState(config.DefaultProfile, aliceLane, []routing.Team{
			orgTeam("org-a", "team-a", "github.com/acme/app"),
			orgTeam("org-b", "team-b", "github.com/beta/app"),
		}, func(s *routing.State, _ map[string]string) error {
			if err := s.Bind("github.com/acme/app", "org-a", false); err != nil {
				return err
			}
			return s.Bind("github.com/beta/app", "org-b", false)
		})
		firstEnr, secondEnr := enr["team-a"], enr["team-b"]
		firstTeam, secondTeam := "team-a", "team-b"
		secondRepo := "github.com/beta/app"
		firstRepo := "github.com/acme/app"
		if secondEnr < firstEnr {
			firstEnr, secondEnr = secondEnr, firstEnr
			firstTeam, secondTeam = secondTeam, firstTeam
			firstRepo, secondRepo = secondRepo, firstRepo
		}

		var mu sync.Mutex
		bodies := map[string]string{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			team := r.Header.Get(sendas.ProjectHeader)
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies[team] = string(b)
			mu.Unlock()
			if team == firstTeam {
				m.updateState(config.DefaultProfile, aliceLane, func(s *routing.State) error {
					t2, ok := s.Team(secondEnr)
					if !ok {
						return errors.New("missing second team")
					}
					pol := t2.Policy
					pol.Revision++
					pol.IncludeToolContent = false
					_, err := s.ApplyRefresh(routing.Refresh{EnrollmentID: secondEnr, Epoch: t2.Epoch, Policy: &pol})
					return err
				})
			}
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)

		ev1 := routedEvent(aliceLane, config.DefaultProfile, firstEnr, firstRepo, semconv.TermaToolCallEvent)
		ev2 := routedEvent(aliceLane, config.DefaultProfile, secondEnr, secondRepo, semconv.TermaToolCallEvent)
		ev2.Attrs[semconv.GenAIToolCallArgumentsKey] = "must-be-stripped-mid-pass"
		m.queueRaw(ev1, ev2)

		r := m.router(&fakeSignIn{account: aliceLane, token: "tok"})
		r.maxConcurrency = 1
		res := r.Flush(
			t.Context(), m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, AuthURL: routedAuth}, true, 0,
		)
		if res.Sent != 2 || res.Err != nil {
			t.Fatalf("Flush = %+v, want Sent=2", res)
		}
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(bodies[secondTeam], "must-be-stripped-mid-pass") {
			t.Fatalf("tightened tool content leaked in second parcel: %s", bodies[secondTeam])
		}
	})

	t.Run("policy expires during credential refresh holds instead of posting", func(t *testing.T) {
		t.Parallel()
		m := newRoutedMachine(t)
		var clockMu sync.Mutex
		now := time.Now()
		tm := orgTeam("org-a", "team-a", "github.com/acme/app")
		// Valid at initial admission, but expires if the clock advances by 10s during token refresh.
		tm.Policy.FetchedAt = now.Add(-config.MaxPolicyAge + 5*time.Second)
		enr := m.createState(config.DefaultProfile, aliceLane, []routing.Team{tm}, func(s *routing.State, _ map[string]string) error {
			return s.Bind("github.com/acme/app", "org-a", false)
		})
		ev := routedEvent(aliceLane, config.DefaultProfile, enr["team-a"], "github.com/acme/app", semconv.TermaToolCallEvent)
		ev.Time = now
		m.queueRaw(ev)

		srv, sent := recordingIngest(t, http.StatusOK)
		signIn := &hookSignIn{
			inner: &fakeSignIn{account: aliceLane, token: "tok-refreshed"},
			onToken: func() {
				clockMu.Lock()
				now = now.Add(10 * time.Second)
				clockMu.Unlock()
			},
		}
		r := m.router(signIn)
		r.now = func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		}
		res := r.Flush(
			t.Context(), m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, AuthURL: routedAuth}, true, 0,
		)
		if res.Sent != 0 || res.Blocked != 1 || res.Held != 1 || res.Err != nil || len(sent()) != 0 {
			t.Fatalf("expired policy during credential refresh must hold without posting: res=%+v sent=%d", res, len(sent()))
		}
	})
}

type hookSignIn struct {
	inner   SignIn
	onToken func()
}

func (h *hookSignIn) Account() string { return h.inner.Account() }
func (h *hookSignIn) Token(ctx context.Context, account string) (string, error) {
	if h.onToken != nil {
		h.onToken()
	}
	return h.inner.Token(ctx, account)
}
func (h *hookSignIn) Refused(ctx context.Context, token string) { h.inner.Refused(ctx, token) }
func (h *hookSignIn) Retired(token string)                      { h.inner.Retired(token) }

// Scheduling isolation: two slow enrollments stalling until the flush deadline must NOT
// starve a healthy sibling enrollment in the same batch.
func TestRoutedSlowEnrollmentsDoNotStarveHealthySibling(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t)
	enr := m.createState(config.DefaultProfile, aliceLane, []routing.Team{
		orgTeam("org-a", "team-slow", "github.com/acme/slow"),
		orgTeam("org-b", "team-fast-1", "github.com/acme/fast1"),
		orgTeam("org-c", "team-fast-2", "github.com/acme/fast2"),
		orgTeam("org-d", "team-fast-3", "github.com/acme/fast3"),
		orgTeam("org-e", "team-fast-4", "github.com/acme/fast4"),
	}, func(s *routing.State, _ map[string]string) error {
		for _, pair := range [][2]string{
			{"github.com/acme/slow", "org-a"},
			{"github.com/acme/fast1", "org-b"},
			{"github.com/acme/fast2", "org-c"},
			{"github.com/acme/fast3", "org-d"},
			{"github.com/acme/fast4", "org-e"},
		} {
			if err := s.Bind(pair[0], pair[1], false); err != nil {
				return err
			}
		}
		return nil
	})

	var mu sync.Mutex
	var fastHits, inFlight, peakInFlight int
	allFastDone := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		inFlight++
		if inFlight > peakInFlight {
			peakInFlight = inFlight
		}
		mu.Unlock()
		defer func() {
			mu.Lock()
			inFlight--
			mu.Unlock()
		}()

		team := r.Header.Get(sendas.ProjectHeader)
		if team != "team-slow" {
			mu.Lock()
			fastHits++
			done := fastHits == 4
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			if done {
				close(allFastDone)
			}
			return
		}
		// Stalled enrollment blocks until all 4 healthy siblings finish — impossible under
		// serial execution or without making progress past maxParcelConcurrency (4) slots.
		select {
		case <-allFastDone:
			w.WriteHeader(http.StatusGatewayTimeout)
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	m.queueRaw(
		routedEvent(aliceLane, config.DefaultProfile, enr["team-slow"], "github.com/acme/slow", semconv.TermaToolCallEvent),
		routedEvent(aliceLane, config.DefaultProfile, enr["team-fast-1"], "github.com/acme/fast1", semconv.TermaToolCallEvent),
		routedEvent(aliceLane, config.DefaultProfile, enr["team-fast-2"], "github.com/acme/fast2", semconv.TermaToolCallEvent),
		routedEvent(aliceLane, config.DefaultProfile, enr["team-fast-3"], "github.com/acme/fast3", semconv.TermaToolCallEvent),
		routedEvent(aliceLane, config.DefaultProfile, enr["team-fast-4"], "github.com/acme/fast4", semconv.TermaToolCallEvent),
	)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	res := m.router(&fakeSignIn{account: aliceLane, token: "shared-tok"}).Flush(
		ctx, m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, AuthURL: routedAuth}, true, 0,
	)
	mu.Lock()
	gotFast, gotPeak := fastHits, peakInFlight
	mu.Unlock()
	if res.Sent != 4 || res.Failed != 1 || len(res.Failures) != 1 || gotFast != 4 || m.pending() != 1 {
		t.Fatalf("healthy siblings must deliver under saturation despite stalled enrollment: res=%+v fastHits=%d pending=%d", res, gotFast, m.pending())
	}
	if gotPeak > maxParcelConcurrency {
		t.Fatalf("peak concurrent parcel sends = %d, exceeds maxParcelConcurrency=%d", gotPeak, maxParcelConcurrency)
	}
}

// HoldKey exact binding check: an event held for a wildcard owner binding (github.com/beta)
// must NOT leave if policy changes to an exact repository entry and only github.com/beta/repo
// is bound while the original owner binding remains pending or refused.
func TestRoutedHoldKeyRequiresPinnedOwnerBinding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		refuseOwner bool
	}{
		{"owner still pending", false},
		{"owner refused with repo override", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := newRoutedMachine(t)
			enr := m.createState(config.DefaultProfile, aliceLane, []routing.Team{
				orgTeam("org-b", "team-b", "github.com/beta/repo"),
			}, func(s *routing.State, _ map[string]string) error {
				if tc.refuseOwner {
					if err := s.Refuse("github.com/beta"); err != nil {
						return err
					}
					return s.Bind("github.com/beta/repo", "org-b", true)
				}
				return s.Bind("github.com/beta/repo", "org-b", false)
			})
			ev := routedEvent(aliceLane, config.DefaultProfile, enr["team-b"], "github.com/beta/repo", semconv.TermaToolCallEvent)
			ev.HoldKey = "github.com/beta" // held for the wildcard owner binding, NOT the exact repo
			m.queueRaw(ev)

			srv, sent := recordingIngest(t, http.StatusOK)
			res := m.router(&fakeSignIn{account: aliceLane, token: "tok"}).Flush(
				t.Context(), m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, AuthURL: routedAuth}, true, 0,
			)
			if res.Sent != 0 || res.Discarded != 1 || len(sent()) != 0 {
				t.Fatalf("unsatisfied owner HoldKey must discard without sending: res=%+v sent=%d", res, len(sent()))
			}
		})
	}
}

// Loss-recording failure: if spool/losses/ cannot be written, destination-less events stay
// queued as undelivered and surface the storage error instead of disappearing uncounted.
func TestRoutedCountLostFailureRetainsDestinationLessEvents(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t)
	m.createState(config.DefaultProfile, aliceLane, []routing.Team{personalTeam()}, nil)

	// Block spool/losses with a regular file so CountLost fails to MkdirAll.
	if err := os.WriteFile(filepath.Join(m.state, spool.Dir, "losses"), []byte("blocker"), 0o600); err != nil {
		t.Fatal(err)
	}
	ev := routedEvent(aliceLane, config.DefaultProfile, "", "github.com/acme/app", semconv.TermaToolCallEvent)
	m.queueRaw(ev)

	srv, sent := recordingIngest(t, http.StatusOK)
	res := m.router(&fakeSignIn{account: aliceLane, token: "tok"}).Flush(
		t.Context(), m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, AuthURL: routedAuth}, true, 0,
	)
	if res.Err == nil || res.Unroutable != 0 || res.Failed != 1 || m.pending() != 1 || len(sent()) != 0 {
		t.Fatalf("CountLost failure must retain event and return error: res=%+v pending=%d sent=%d", res, m.pending(), len(sent()))
	}
}

// Cross-profile delivery: an event queued under another profile of the same account must
// resolve its endpoint and environment from that event's own profile (not the active
// profile's OTLPURL), respect a process-wide OTLPPinned override, and fail closed when
// the event's profile does not exist in config.json.
func TestRoutedCrossProfileUsesEventProfileEndpoint(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t)
	defaultSrv, defaultSent := recordingIngest(t, http.StatusOK)
	workSrv, workSent := recordingIngest(t, http.StatusOK)
	pinnedSrv, pinnedSent := recordingIngest(t, http.StatusOK)

	if err := config.UpdateProfile(m.config, "work", func(p *config.Profile) {
		p.AuthURL = routedAuth
		p.OTLPURL = workSrv.URL
	}); err != nil {
		t.Fatal(err)
	}
	enrDefault := m.createState(config.DefaultProfile, aliceLane, []routing.Team{
		orgTeam("org-a", "team-default", "github.com/acme/default"),
	}, func(s *routing.State, _ map[string]string) error {
		return s.Bind("github.com/acme/default", "org-a", false)
	})
	enrWork := m.createState("work", aliceLane, []routing.Team{
		orgTeam("org-b", "team-work", "github.com/acme/work"),
	}, func(s *routing.State, _ map[string]string) error {
		return s.Bind("github.com/acme/work", "org-b", false)
	})
	enrMissing := m.createState("deleted", aliceLane, []routing.Team{
		orgTeam("org-c", "team-deleted", "github.com/acme/deleted"),
	}, func(s *routing.State, _ map[string]string) error {
		return s.Bind("github.com/acme/deleted", "org-c", false)
	})

	m.queueRaw(
		routedEvent(aliceLane, config.DefaultProfile, enrDefault["team-default"], "github.com/acme/default", semconv.TermaToolCallEvent),
		routedEvent(aliceLane, "work", enrWork["team-work"], "github.com/acme/work", semconv.TermaToolCallEvent),
		routedEvent(aliceLane, "deleted", enrMissing["team-deleted"], "github.com/acme/deleted", semconv.TermaToolCallEvent),
	)

	cfg := &config.Config{OTLPURL: defaultSrv.URL, ProfileName: config.DefaultProfile, AuthURL: routedAuth}
	res := m.router(&fakeSignIn{account: aliceLane, token: "tok"}).Flush(t.Context(), m.spool, cfg, true, 0)
	if res.Sent != 2 || res.Held != 1 || res.Err != nil {
		t.Fatalf("cross-profile Flush = %+v, want Sent=2 Held=1", res)
	}
	if len(defaultSent()) != 1 || defaultSent()[0].project != "team-default" {
		t.Fatalf("default endpoint received %+v, want only team-default", defaultSent())
	}
	if len(workSent()) != 1 || workSent()[0].project != "team-work" {
		t.Fatalf("work endpoint received %+v, want only team-work", workSent())
	}

	// Process-wide OTLPPinned override wins over the event profile's OTLPURL.
	m.queueRaw(routedEvent(aliceLane, "work", enrWork["team-work"], "github.com/acme/work", semconv.TermaToolCallEvent))
	rPinned := m.router(&fakeSignIn{account: aliceLane, token: "tok"})
	rPinned.OTLPPinned = true
	resPinned := rPinned.Flush(t.Context(), m.spool, &config.Config{
		OTLPURL: pinnedSrv.URL, ProfileName: config.DefaultProfile, AuthURL: routedAuth,
	}, true, 0)
	if resPinned.Sent != 1 || len(pinnedSent()) != 1 || len(workSent()) != 1 {
		t.Fatalf("OTLPPinned Flush = %+v, pinned=%d work=%d", resPinned, len(pinnedSent()), len(workSent()))
	}
}

// Fail closed in pre-activation byProject: if routing state is deleted, an event carrying
// routed Profile metadata (even with empty Enrollment) must NOT fall through to legacy
// attrs.project_id delivery.
func TestPreActivationFailsClosedOnProfileOnlyRoutedEvent(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t)
	ev := routedEvent(aliceLane, config.DefaultProfile, "", "github.com/acme/app", semconv.TermaToolCallEvent)
	ev.Attrs[hookrun.AttrProjectID] = "legacy-team"
	m.queueRaw(ev)

	srv, sent := recordingIngest(t, http.StatusOK)
	r := m.router(&fakeSignIn{account: aliceLane, token: "tok"})
	res := r.Flush(t.Context(), m.spool, &config.Config{
		OTLPURL: srv.URL, ProfileName: config.DefaultProfile, AuthURL: routedAuth,
	}, true, 0)
	if res.Routed || res.Sent != 0 || res.Unrouted != 1 || res.Held != 1 || len(sent()) != 0 {
		t.Fatalf("profile-only routed event must fail closed without routing state: res=%+v sent=%d", res, len(sent()))
	}
}

// Idempotent no_destination loss recording:
//  1. A canceled flush must not record premature losses.
//  2. A crash between ImportLosses and ackBatch must deduplicate per event even when
//     intervening hooks append distinct or identical events before retry.
//  3. Expired imported~nodest-* loss files are pruned independently (and throttled).
//  4. Byte-identical events across spool batch boundaries (>200 events) and across
//     separate flushes get distinct queue-file identities and never undercount.
func TestRoutedNoDestinationLossIsIdempotentOnCanceledFlush(t *testing.T) {
	t.Parallel()
	m := newRoutedMachine(t)
	enr := m.createState(config.DefaultProfile, aliceLane, []routing.Team{
		orgTeam("org-a", "team-slow", "github.com/acme/app"),
	}, func(s *routing.State, _ map[string]string) error {
		return s.Bind("github.com/acme/app", "org-a", false)
	})

	release := make(chan struct{})
	stalledEntered := make(chan struct{}, 1)
	var stall atomic.Bool
	stall.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if stall.Load() {
			select {
			case stalledEntered <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	evA := routedEvent(aliceLane, config.DefaultProfile, "", "github.com/acme/app", semconv.TermaToolCallEvent)
	m.queueRaw(
		routedEvent(aliceLane, config.DefaultProfile, enr["team-slow"], "github.com/acme/app", semconv.TermaToolCallEvent),
		evA,
	)

	// Pass 1: flush context is canceled while team-slow is in-flight -> batch cannot ack,
	// and no premature no_destination loss is written.
	ctx1, cancel1 := context.WithCancel(t.Context())
	go func() {
		<-stalledEntered
		cancel1()
	}()
	cfg := &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, AuthURL: routedAuth}
	r := m.router(&fakeSignIn{account: aliceLane, token: "tok"})
	res1 := r.Flush(ctx1, m.spool, cfg, true, 0)
	if res1.Err == nil || m.pending() != 2 {
		t.Fatalf("canceled pass 1 = %+v, pending=%d", res1, m.pending())
	}
	spoolDir := filepath.Join(m.state, spool.Dir)
	if losses, err := spool.ReadLosses(spoolDir, time.Now()); err != nil || losses.Counts[LossNoDestination] != 0 {
		t.Fatalf("canceled pass must not record premature no_destination loss: losses=%+v err=%v", losses, err)
	}

	// Simulate a crash right after ImportLosses for evA (before ackBatch), followed by
	// intervening hooks appending a distinct event evB and an identical copy of evA before
	// the retry flush reads all three together as one batch.
	crashed := &pass{r: r, s: m.spool}
	if err := crashed.recordNoDestination(t.Context(), []spool.Event{evA}); err != nil {
		t.Fatalf("simulated pre-crash recordNoDestination: %v", err)
	}
	evB := routedEvent(aliceLane, config.DefaultProfile, "", "github.com/acme/other", semconv.TermaToolCallEvent)
	m.queueRaw(evB, evA)

	// Also plant an expired imported~nodest-* loss file (and clear throttle marker) to
	// prove independent retention pruning runs.
	stalePath := filepath.Join(spoolDir, "losses", "imported~nodest-stale.jsonl")
	if err := os.WriteFile(stalePath, spool.EncodeLoss(LossNoDestination, 1, time.Now().Add(-spool.LossWindow-48*time.Hour)), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-spool.LossWindow - 48*time.Hour)
	if err := os.Chtimes(stalePath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(spoolDir, "losses", noDestPruneMarker))

	// Pass 2: endpoint recovers -> spool replays [team-slow, evA, evB, evA] in one batch.
	// evA-0 overwrites the pre-crash file with identical bytes; evB-0 and evA-1 are new,
	// yielding exactly 3 no_destination losses for 3 dropped events, and stale file is pruned.
	stall.Store(false)
	res2 := r.Flush(t.Context(), m.spool, cfg, true, 0)
	if res2.Sent != 1 || res2.Unroutable != 3 || res2.Err != nil || m.pending() != 0 {
		t.Fatalf("recovery pass 2 = %+v, pending=%d", res2, m.pending())
	}
	if losses, err := spool.ReadLosses(spoolDir, time.Now()); err != nil || losses.Counts[LossNoDestination] != 3 {
		t.Fatalf("ReadLosses after intervening-append replay = %+v (err=%v), want 3", losses, err)
	}
	if taken, err := spool.TakeNewLosses(spoolDir); err != nil || taken[LossNoDestination] != 3 {
		t.Fatalf("TakeNewLosses after intervening-append replay = %v (err=%v), want 3", taken, err)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("expired imported~nodest-* loss file must be pruned, stat err = %v", err)
	}

	// Pass 3: 201 byte-identical events spanning two default spool batches (200 + 1) must
	// all be counted without collision because ackBatch atomically replaces events.jsonl.
	identicalBatch := make([]spool.Event, 201)
	for i := range identicalBatch {
		identicalBatch[i] = evA
	}
	m.queueRaw(identicalBatch...)
	res3 := r.Flush(t.Context(), m.spool, cfg, true, 0)
	if res3.Unroutable != 201 || res3.Err != nil || m.pending() != 0 {
		t.Fatalf("multi-batch identical pass 3 = %+v, pending=%d", res3, m.pending())
	}
	if taken, err := spool.TakeNewLosses(spoolDir); err != nil || taken[LossNoDestination] != 201 {
		t.Fatalf("TakeNewLosses for 201 identical events across batches = %v (err=%v), want 201", taken, err)
	}

	// Pass 4: another byte-identical event in a separate subsequent flush must also count.
	m.queueRaw(evA)
	res4 := r.Flush(t.Context(), m.spool, cfg, true, 0)
	if res4.Unroutable != 1 || res4.Err != nil || m.pending() != 0 {
		t.Fatalf("separate-flush identical pass 4 = %+v, pending=%d", res4, m.pending())
	}
	if taken, err := spool.TakeNewLosses(spoolDir); err != nil || taken[LossNoDestination] != 1 {
		t.Fatalf("TakeNewLosses for separate-flush identical event = %v (err=%v), want 1", taken, err)
	}
	if losses, err := spool.ReadLosses(spoolDir, time.Now()); err != nil || losses.Counts[LossNoDestination] != 205 {
		t.Fatalf("total ReadLosses = %+v (err=%v), want 205 (3+201+1)", losses, err)
	}
}

// Full-slot saturation + early-403/late-cancellation:
//  1. When all maxParcelConcurrency (4) worker slots are simultaneously occupied by stalled
//     enrollments under a sub-second (<1s) remaining deadline, proportional ack headroom
//     and wave budgeting still let a 5th queued healthy enrollment deliver and commit durably.
//  2. When one enrollment fails early with 403 ORG_NOT_ACCESSIBLE and a stalled sibling
//     remains in-flight until the flush context is canceled, the 403 enrollment still
//     persists its per-enrollment retry backoff without retiring the shared user token.
func TestRoutedFullSlotSaturationAndEarly403Backoff(t *testing.T) {
	t.Parallel()

	t.Run("full-slot saturation under sub-second deadline progresses queued healthy parcel", func(t *testing.T) {
		t.Parallel()
		m := newRoutedMachine(t)
		teams := []routing.Team{
			orgTeam("org-1", "team-1", "github.com/acme/r1"),
			orgTeam("org-2", "team-2", "github.com/acme/r2"),
			orgTeam("org-3", "team-3", "github.com/acme/r3"),
			orgTeam("org-4", "team-4", "github.com/acme/r4"),
			orgTeam("org-5", "team-5", "github.com/acme/r5"),
		}
		enr := m.createState(config.DefaultProfile, aliceLane, teams, func(s *routing.State, _ map[string]string) error {
			for i, tm := range teams {
				if err := s.Bind(fmt.Sprintf("github.com/acme/r%d", i+1), tm.OrganizationID, false); err != nil {
					return err
				}
			}
			return nil
		})
		// Pick the lexicographically last enrollment ID as the healthy team so the 4
		// stalled enrollments acquire all 4 FIFO semaphore slots first.
		fastTeam := ""
		maxEnr := ""
		for _, tm := range teams {
			if enr[tm.TeamID] > maxEnr {
				maxEnr = enr[tm.TeamID]
				fastTeam = tm.TeamID
			}
		}

		var mu sync.Mutex
		var stalledNow, maxStalled int
		allSaturated := make(chan struct{})
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			if r.Header.Get(sendas.ProjectHeader) == fastTeam {
				w.WriteHeader(http.StatusOK)
				return
			}
			mu.Lock()
			stalledNow++
			if stalledNow > maxStalled {
				maxStalled = stalledNow
			}
			if stalledNow == maxParcelConcurrency {
				select {
				case <-allSaturated:
				default:
					close(allSaturated)
				}
			}
			mu.Unlock()
			defer func() {
				mu.Lock()
				stalledNow--
				mu.Unlock()
			}()
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		t.Cleanup(func() {
			close(release)
			srv.Close()
		})

		for i, tm := range teams {
			m.queueRaw(routedEvent(aliceLane, config.DefaultProfile, enr[tm.TeamID], fmt.Sprintf("github.com/acme/r%d", i+1), semconv.TermaToolCallEvent))
		}

		// Sub-second deadline (800ms) proves proportional headroom and wave budgeting hold
		// even on late batches of a flush with < 1s remaining.
		ctx, cancel := context.WithTimeout(t.Context(), 800*time.Millisecond)
		defer cancel()
		res := m.router(&fakeSignIn{account: aliceLane, token: "shared-tok"}).Flush(
			ctx, m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, AuthURL: routedAuth}, true, 0,
		)
		mu.Lock()
		gotMaxStalled := maxStalled
		mu.Unlock()
		if gotMaxStalled != maxParcelConcurrency {
			t.Fatalf("expected all %d worker slots saturated by stalled parcels, got peak %d", maxParcelConcurrency, gotMaxStalled)
		}
		if res.Sent != 1 || res.Failed != 4 || m.pending() != 4 {
			t.Fatalf("full-slot saturation must durably commit queued healthy parcel: res=%+v pending=%d", res, m.pending())
		}
	})

	t.Run("early 403 keeps retry backoff despite sibling context cancellation", func(t *testing.T) {
		t.Parallel()
		m := newRoutedMachine(t)
		enr := m.createState(config.DefaultProfile, aliceLane, []routing.Team{
			orgTeam("org-403", "team-403", "github.com/acme/denied"),
			orgTeam("org-stall", "team-stall", "github.com/acme/stall"),
		}, func(s *routing.State, _ map[string]string) error {
			if err := s.Bind("github.com/acme/denied", "org-403", false); err != nil {
				return err
			}
			return s.Bind("github.com/acme/stall", "org-stall", false)
		})

		got403 := make(chan struct{}, 1)
		stallEntered := make(chan struct{}, 1)
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			if r.Header.Get(sendas.ProjectHeader) == "team-403" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"ORG_NOT_ACCESSIBLE"}`))
				select {
				case got403 <- struct{}{}:
				default:
				}
				return
			}
			select {
			case stallEntered <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		t.Cleanup(func() {
			close(release)
			srv.Close()
		})

		m.queueRaw(
			routedEvent(aliceLane, config.DefaultProfile, enr["team-403"], "github.com/acme/denied", semconv.TermaToolCallEvent),
			routedEvent(aliceLane, config.DefaultProfile, enr["team-stall"], "github.com/acme/stall", semconv.TermaToolCallEvent),
		)

		si := &fakeSignIn{account: aliceLane, token: "shared-tok"}
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			<-got403
			<-stallEntered
			// Give team-403's HTTP client a brief moment to read the 403 response before
			// canceling the shared flush context while team-stall is still blocked.
			time.Sleep(30 * time.Millisecond)
			cancel()
		}()
		r := m.router(si)
		res := r.Flush(ctx, m.spool, &config.Config{OTLPURL: srv.URL, ProfileName: config.DefaultProfile, AuthURL: routedAuth}, true, 0)
		var retry403 time.Time
		for _, f := range res.Failures {
			if f.ProjectID == "team-403" {
				retry403 = f.RetryAt
			}
		}
		if retry403.IsZero() {
			t.Fatalf("early 403 must still record per-enrollment RetryAt when sibling cancels ctx: failures=%+v", res.Failures)
		}
		if len(si.refused) != 0 || si.retired {
			t.Fatalf("403 ORG_NOT_ACCESSIBLE must not call Refused/Retired: refused=%v retired=%v", si.refused, si.retired)
		}
		if m.spool.RetryAt(retryDest(enr["team-403"])).IsZero() {
			t.Fatalf("team-403 retry window was not persisted in spool")
		}
	})
}
