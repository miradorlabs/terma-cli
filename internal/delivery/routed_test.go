package delivery

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/account/secret"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// routedAuth is the sign-in environment every routed test team was fetched in.
const routedAuth = "https://auth.example"

// machine is a migrated machine: its config and state directories, and its hooks' spool.
type machine struct {
	t             *testing.T
	config, state string
	spool         *spool.Spool
}

func migratedMachine(t *testing.T) *machine {
	t.Helper()
	m := &machine{t: t, config: t.TempDir(), state: t.TempDir()}
	if err := os.WriteFile(config.CredentialsV2Path(m.config), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := spool.Open(filepath.Join(m.state, spool.Dir))
	if err != nil {
		t.Fatal(err)
	}
	m.spool = s
	return m
}

// orgTeam is a team of org whose policy, validated an hour ago, lists entries.
func orgTeam(org, id string, entries ...string) routing.Team {
	return routing.Team{OrganizationID: org, AuthURL: routedAuth, TeamID: id, Policy: config.Policy{
		Mode: config.ModeRepo, TeamID: id, OrganizationID: org, AuthURL: routedAuth, Repositories: entries,
		Wildcards: true, Revision: 1, FetchedAt: time.Now().Add(-time.Hour),
	}}
}

// personalTeam collects everything not bound to an organization.
func personalTeam() routing.Team {
	t := orgTeam("me", "personal")
	t.Personal = true
	t.Policy.Mode = config.ModeGlobal
	return t
}

// route creates profile's routing state with teams selected in order, then change, and
// returns each team's enrollment by team ID.
func (m *machine) route(profile string, teams []routing.Team, change func(*routing.State, map[string]string) error) map[string]string {
	m.t.Helper()
	enrolled := map[string]string{}
	_, err := routing.Create(m.state, profile, func(s *routing.State) error {
		for _, tm := range teams {
			id, err := s.Select(tm)
			if err != nil {
				return err
			}
			enrolled[tm.TeamID] = id
		}
		if change == nil {
			return nil
		}
		return change(s, enrolled)
	})
	if err != nil {
		m.t.Fatal(err)
	}
	return enrolled
}

func (m *machine) update(profile string, change func(*routing.State) error) {
	m.t.Helper()
	if _, err := routing.Update(m.state, profile, change); err != nil {
		m.t.Fatal(err)
	}
}

// key files enrollment's team key, for the host it belongs to.
func (m *machine) key(enrollment, project, key string, host *ingest) {
	m.t.Helper()
	if err := keystore.SetTeam(m.config, enrollment, project, key, keystore.Hosts{OTLP: host.URL}); err != nil {
		m.t.Fatal(err)
	}
}

func (m *machine) queue(events ...spool.Event) {
	m.t.Helper()
	if err := m.spool.Append(events...); err != nil {
		m.t.Fatal(err)
	}
}

func (m *machine) pending() int {
	n, _, _ := m.spool.Pending()
	return n
}

// router has no Policy: a migrated pass that asked for one would panic.
func (m *machine) router() Router {
	return Router{ConfigDir: m.config, StateDir: m.state}
}

// flush is one forced pass, with the profile's own ingest a host no one listens on.
func (m *machine) flush(r Router) Result {
	return r.Flush(m.t.Context(), m.spool, &config.Config{ProfileName: config.DefaultProfile, OTLPURL: "http://127.0.0.1:1"}, true, 0)
}

// routedEvent is a hook event that ran in origin ("" outside git), spooled by the default
// profile for enrollment, its session's turn marking it.
func routedEvent(enrollment, origin, session, mark string) spool.Event {
	return spool.Event{Time: time.Now(), Name: semconv.TermaSessionStartEvent, SessionID: session,
		Repository: config.Repository{Origin: origin}, Attrs: map[string]any{semconv.TermaTurnIDKey: mark},
		Profile: config.DefaultProfile, Enrollment: enrollment}
}

func bindAcme(s *routing.State, _ map[string]string) error {
	return s.Bind("github.com/acme", "acme", false)
}

// ingest records what one environment's ingest host is sent.
type ingest struct {
	*httptest.Server
	mu           sync.Mutex
	auth, bodies []string
}

func newIngest(t *testing.T) *ingest {
	t.Helper()
	h := &ingest{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		defer h.mu.Unlock()
		h.auth = append(h.auth, r.Header.Get("Authorization"))
		h.bodies = append(h.bodies, string(b))
	}))
	t.Cleanup(h.Close)
	return h
}

// sent is every request's key and everything sent, joined.
func (h *ingest) sent() (keys []string, body string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Compact(slices.Sorted(slices.Values(h.auth))), strings.Join(h.bodies, "\n")
}

// Each event goes to the enrollment it was spooled with, with that enrollment's key to its
// host, and names the user EnrolledUser names for it. One spooled for a team that no longer
// covers its repository is dropped, never sent to the team that would collect it now, and
// the key the legacy index holds for the same team is never used.
func TestMigratedEventsGoOnlyToTheirOwnEnrollment(t *testing.T) {
	t.Parallel()
	m := migratedMachine(t)
	acmeHost, personalHost, legacyHost := newIngest(t), newIngest(t), newIngest(t)
	e := m.route(config.DefaultProfile, []routing.Team{orgTeam("acme", "acme-platform", "github.com/acme/*"), personalTeam()}, bindAcme)
	m.key(e["acme-platform"], "acme-platform", "ter_srv_acme", acmeHost)
	m.key(e["personal"], "personal", "ter_srv_personal", personalHost)
	if err := keystore.Set(m.config, "acme-platform", "ter_srv_legacy", keystore.Hosts{OTLP: legacyHost.URL}); err != nil {
		t.Fatal(err)
	}
	m.queue(
		routedEvent(e["acme-platform"], "github.com/acme/api", "s-acme", "acme-turn"),
		routedEvent(e["personal"], "", "s-scratch", "scratch-turn"),
		routedEvent(e["acme-platform"], "github.com/dawson/side", "s-side", "side-turn"),
	)
	r := m.router()
	var asked []string
	r.EnrolledUser = func(profile string, team routing.Team, hosts keystore.Hosts, endpoint string) string {
		asked = append(asked, profile+" "+team.TeamID+" "+hosts.OTLP+" "+endpoint)
		if team.TeamID == "acme-platform" {
			return "user-a"
		}
		return ""
	}
	res := m.flush(r)
	if res.Sent != 2 || res.Discarded != 1 || !res.Migrated || res.Err != nil || m.pending() != 0 {
		t.Fatalf("Flush = %+v, pending %d", res, m.pending())
	}
	keys, body := acmeHost.sent()
	if !slices.Equal(keys, []string{"Bearer ter_srv_acme"}) || !strings.Contains(body, "acme-turn") || strings.Contains(body, "side-turn") {
		t.Errorf("Acme's host was sent %v:\n%s", keys, body)
	}
	if !strings.Contains(body, `"stringValue":"user-a"`) || !strings.Contains(body, `"stringValue":"acme-platform"`) {
		t.Errorf("Acme's events left without their user or team:\n%s", body)
	}
	keys, body = personalHost.sent()
	if !slices.Equal(keys, []string{"Bearer ter_srv_personal"}) || !strings.Contains(body, "scratch-turn") || strings.Contains(body, "side-turn") || strings.Contains(body, semconv.TermaUserIDKey) {
		t.Errorf("personal's host was sent %v:\n%s", keys, body)
	}
	if keys, _ := legacyHost.sent(); len(keys) != 0 {
		t.Errorf("the legacy key's host was sent %v", keys)
	}
	slices.Sort(asked)
	want := []string{"default acme-platform " + acmeHost.URL + " " + acmeHost.URL, "default personal " + personalHost.URL + " " + personalHost.URL}
	if !slices.Equal(asked, want) {
		t.Errorf("asked who events name for %q, want %q", asked, want)
	}
}

// The recheck table: an event whose team may still recover stays in the spool and is sent
// once it does; one whose team is Refused, unticked or no longer lists it is dropped.
func TestARecheckHoldsOnlyWhileTheTeamMayRecover(t *testing.T) {
	t.Parallel()
	expire := func(s *routing.State) error {
		s.Teams[0].Policy.FetchedAt = time.Now().Add(-config.MaxPolicyAge - time.Hour)
		return nil
	}
	for _, tc := range []struct {
		name            string
		change, recover func(*routing.State) error
	}{
		{"expired", expire, func(s *routing.State) error { s.Teams[0].Policy.FetchedAt = time.Now(); return nil }},
		{"signing in", func(s *routing.State) error { s.Teams[0].SignInNeeded = true; return nil },
			func(s *routing.State) error { s.Teams[0].SignInNeeded = false; return nil }},
		{"suspended", func(s *routing.State) error { s.Teams[0].Refusals = 1; return nil },
			func(s *routing.State) error { s.Teams[0].Refusals = 0; return nil }},
		{"refused", func(s *routing.State) error { s.Teams[0].Refusals, s.Teams[0].RefusedAt = 2, time.Now(); return nil }, nil},
		{"unticked", func(s *routing.State) error { s.Deselect(s.Teams[0].EnrollmentID); return nil }, nil},
		{"no longer listed", func(s *routing.State) error {
			s.Teams[0].Policy.Repositories = []string{"github.com/acme/web"}
			return nil
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := migratedMachine(t)
			host := newIngest(t)
			e := m.route(config.DefaultProfile, []routing.Team{orgTeam("acme", "acme-platform", "github.com/acme/*")}, bindAcme)
			m.key(e["acme-platform"], "acme-platform", "ter_srv_acme", host)
			m.queue(routedEvent(e["acme-platform"], "github.com/acme/api", "s1", "turn"))
			m.update(config.DefaultProfile, tc.change)
			res := m.flush(m.router())
			if keys, _ := host.sent(); len(keys) != 0 || res.Sent != 0 {
				t.Fatalf("sent %v: %+v", keys, res)
			}
			if tc.recover == nil {
				if res.Discarded != 1 || m.pending() != 0 {
					t.Fatalf("want it dropped: %+v, pending %d", res, m.pending())
				}
				return
			}
			if res.Blocked != 1 || res.Held != 1 || m.pending() != 1 {
				t.Fatalf("want it held: %+v, pending %d", res, m.pending())
			}
			m.update(config.DefaultProfile, tc.recover)
			if res := m.flush(m.router()); res.Sent != 1 || m.pending() != 0 {
				t.Fatalf("once recovered: %+v, pending %d", res, m.pending())
			}
		})
	}
}

// An event held for a pending binding is sent only once that binding is bound to the
// holding team's organization; decided any other way, it is dropped, personal taking none.
func TestAPendingHoldSendsOnlyOnceBoundToTheHoldingOrganization(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		decide func(*routing.State) error
		sent   bool
	}{
		{"bound to the holding organization", func(s *routing.State) error { return s.Bind("github.com/torvalds/linux", "acme", false) }, true},
		{"bound to another", func(s *routing.State) error { return s.Bind("github.com/torvalds/linux", "clientco", false) }, false},
		{"refused", func(s *routing.State) error { return s.Refuse("github.com/torvalds/linux") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := migratedMachine(t)
			acmeHost, personalHost := newIngest(t), newIngest(t)
			e := m.route(config.DefaultProfile, []routing.Team{
				orgTeam("acme", "acme-platform", "github.com/acme/*", "github.com/torvalds/linux"), personalTeam(),
			}, bindAcme)
			m.key(e["acme-platform"], "acme-platform", "ter_srv_acme", acmeHost)
			m.key(e["personal"], "personal", "ter_srv_personal", personalHost)
			held := routedEvent(e["acme-platform"], "github.com/torvalds/linux", "s1", "linux-turn")
			held.HoldKey = "github.com/torvalds/linux"
			m.queue(held)
			if res := m.flush(m.router()); res.Blocked != 1 || res.Sent != 0 || m.pending() != 1 {
				t.Fatalf("while pending: %+v, pending %d", res, m.pending())
			}
			m.update(config.DefaultProfile, tc.decide)
			res := m.flush(m.router())
			acmeKeys, _ := acmeHost.sent()
			personalKeys, _ := personalHost.sent()
			if m.pending() != 0 || len(personalKeys) != 0 || (len(acmeKeys) == 1) != tc.sent || (res.Sent == 1) != tc.sent || (res.Discarded == 1) == tc.sent {
				t.Fatalf("Flush = %+v; Acme sent %v, personal %v, pending %d", res, acmeKeys, personalKeys, m.pending())
			}
		})
	}
}

// A run that crossed organizations drops the events attributed through it from the moment
// it did, under the session's ID and its subagents'; an event with its own location, and
// one from before the crossing, still go.
func TestAMixedRunDropsItsRunAttributedEvents(t *testing.T) {
	t.Parallel()
	m := migratedMachine(t)
	host := newIngest(t)
	e := m.route(config.DefaultProfile, []routing.Team{orgTeam("acme", "acme-platform", "github.com/acme/*")}, bindAcme)
	m.key(e["acme-platform"], "acme-platform", "ter_srv_acme", host)
	began := time.Now().Add(-time.Hour)
	run, err := runs.Start(m.state, "sess-1", began)
	if err != nil {
		t.Fatal(err)
	}
	crossed := began.Add(30 * time.Minute)
	if err := runs.MarkMixed(m.state, "sess-1", crossed); err != nil {
		t.Fatal(err)
	}
	if err := runs.Alias(m.state, "sess-1", "sub-1"); err != nil {
		t.Fatal(err)
	}
	at := func(mark, session string, when time.Time, own bool) spool.Event {
		ev := routedEvent(e["acme-platform"], "github.com/acme/api", session, mark)
		ev.Time, ev.Run, ev.OwnLocation = when, run, own
		return ev
	}
	m.queue(
		at("before", "sess-1", crossed.Add(-time.Minute), false),
		at("at-the-crossing", "sess-1", crossed, false),
		at("after", "sess-1", crossed.Add(time.Minute), false),
		at("subagent-after", "sub-1", crossed.Add(time.Minute), false),
		at("own-location", "sess-1", crossed.Add(time.Minute), true),
	)
	res := m.flush(m.router())
	if res.Sent != 2 || res.Mixed != 3 || m.pending() != 0 {
		t.Fatalf("Flush = %+v, pending %d", res, m.pending())
	}
	_, body := host.sent()
	for _, mark := range []string{"before", "own-location"} {
		if !strings.Contains(body, `"`+mark+`"`) {
			t.Errorf("%s was not sent:\n%s", mark, body)
		}
	}
	for _, mark := range []string{"at-the-crossing", `"after"`, "subagent-after"} {
		if strings.Contains(body, mark) {
			t.Errorf("%s was sent after its run crossed:\n%s", mark, body)
		}
	}
}

// A team's events need its own enrollment's key: with none yet they are held, and with the
// keychain locked they stay queued and the pass names why.
func TestAnEnrollmentsKeyIsTheOnlyOneUsed(t *testing.T) {
	t.Parallel()
	m := migratedMachine(t)
	host := newIngest(t)
	e := m.route(config.DefaultProfile, []routing.Team{orgTeam("acme", "acme-platform", "github.com/acme/*")}, bindAcme)
	if err := keystore.Set(m.config, "acme-platform", "ter_srv_legacy", keystore.Hosts{OTLP: host.URL}); err != nil {
		t.Fatal(err)
	}
	m.queue(routedEvent(e["acme-platform"], "github.com/acme/api", "s1", "turn"))
	if res := m.flush(m.router()); res.Held != 1 || res.Blocked != 0 || res.Sent != 0 || m.pending() != 1 {
		t.Fatalf("with no key of its own: %+v, pending %d", res, m.pending())
	}
	m.key(e["acme-platform"], "acme-platform", "ter_srv_acme", host)
	secret.FailForTest(t, m.config)
	res := m.flush(m.router())
	if keys, _ := host.sent(); len(keys) != 0 || res.Sent != 0 || !secret.IsUnavailable(res.Err) || m.pending() != 1 {
		t.Fatalf("with the keychain locked: %+v, sent %v, pending %d", res, keys, m.pending())
	}
}

// Delivery's own checks stay: a reply leaves only under its team's prompts and its agent's
// consent, and the team's content rules now strip what they withhold.
func TestMigratedDeliveryKeepsItsContentChecks(t *testing.T) {
	t.Parallel()
	m := migratedMachine(t)
	host := newIngest(t)
	e := m.route(config.DefaultProfile, []routing.Team{orgTeam("acme", "acme-platform", "github.com/acme/*")}, bindAcme)
	m.key(e["acme-platform"], "acme-platform", "ter_srv_acme", host)
	reply := routedEvent(e["acme-platform"], "github.com/acme/api", "s1", "reply")
	reply.Name, reply.Attrs[semconv.GenAIMainAgentNameKey] = semconv.TermaAssistantMessageEvent, "fake"
	call := routedEvent(e["acme-platform"], "github.com/acme/api", "s1", "call")
	call.Name, call.Attrs[semconv.GenAIToolCallArgumentsKey] = semconv.TermaToolCallEvent, "private arguments"
	consenting := m.router()
	consenting.Consent = func(string, hookrun.Consent) bool { return true }

	m.queue(reply, call)
	if res := m.flush(consenting); res.Sent != 1 || res.Withheld != 1 {
		t.Fatalf("prompts off: %+v", res)
	}
	m.update(config.DefaultProfile, func(s *routing.State) error { s.Teams[0].Policy.IncludePrompts = true; return nil })
	m.queue(reply)
	if res := m.flush(m.router()); res.Sent != 0 || res.Withheld != 1 {
		t.Fatalf("no one to consent: %+v", res)
	}
	m.queue(reply)
	if res := m.flush(consenting); res.Sent != 1 {
		t.Fatalf("prompts on and consented: %+v", res)
	}
	_, body := host.sent()
	if strings.Contains(body, "private arguments") || !strings.Contains(body, `"call"`) || strings.Count(body, `"reply"`) != 1 {
		t.Fatalf("sent:\n%s", body)
	}
}
