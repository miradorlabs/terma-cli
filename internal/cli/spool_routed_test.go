package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/delivery"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

func TestDescribeFlushRoutedClauses(t *testing.T) {
	delivered, undelivered := describeFlush(delivery.Result{
		Routed:     true,
		Sent:       2,
		Held:       6,
		Blocked:    2,
		Unrouted:   1,
		Unroutable: 3,
		Discarded:  4,
		Mixed:      5,
		MixedBy:    map[runs.Reason]int{runs.Crossed: 3, runs.UnknownReach: 2},
	}, "terma setup")
	if delivered != "2 events" {
		t.Fatalf("delivered = %q", delivered)
	}
	want := []string{
		"holding 3 for a sign-in that may send them (run `terma setup`)",
		"holding 2 until their team can send again (run `terma doctor`)",
		"holding 1 until this machine's routing state can be read (run `terma doctor`)",
		"dropped 3 with no destination",
		"dropped 4 their team no longer collects",
		"dropped 5 because their session's run is Mixed (3 crossed organizations, 2 ran a shell command whose reach is not known)",
	}
	if strings.Join(undelivered, "|") != strings.Join(want, "|") {
		t.Fatalf("undelivered =\n  %q\nwant\n  %q", undelivered, want)
	}
}

func TestCLISpoolFlushRoutedUserTokenAndEnrolledUser(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := saveSignIn(testApp.dir, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	account := auth.AccountKey(f.srv.URL, "u-1")

	// Set a stale single-team policy on config.json to prove routed flush skips refreshOnce.
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) {
		p.Team = "team-old-single"
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	type reqSeen struct{ auth, project, body string }
	var seen []reqSeen
	ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, reqSeen{r.Header.Get("Authorization"), r.Header.Get(sendas.ProjectHeader), string(b)})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ingest.Close)
	t.Setenv("TERMA_OTLP_URL", ingest.URL)

	enr := map[string]string{}
	_, err := routing.Create(testApp.stateDir, config.DefaultProfile, account, func(s *routing.State) error {
		teamA := routing.Team{
			OrganizationID: "org-a", AuthURL: f.srv.URL, TeamID: "team-a",
			Policy: config.Policy{
				Mode: config.ModeRepo, TeamID: "team-a", OrganizationID: "org-a", AuthURL: f.srv.URL,
				Repositories: []string{"github.com/acme/app"}, Revision: 1, FetchedAt: time.Now(),
			},
		}
		teamB := routing.Team{
			OrganizationID: "org-b", AuthURL: f.srv.URL, TeamID: "team-b",
			Policy: config.Policy{
				Mode: config.ModeRepo, TeamID: "team-b", OrganizationID: "org-b", AuthURL: f.srv.URL,
				Repositories: []string{"github.com/beta/lib"}, Revision: 1, FetchedAt: time.Now(),
			},
		}
		var err error
		if enr["team-a"], err = s.Select(teamA); err != nil {
			return err
		}
		if enr["team-b"], err = s.Select(teamB); err != nil {
			return err
		}
		if err := s.Bind("github.com/acme/app", "org-a", false); err != nil {
			return err
		}
		return s.Bind("github.com/beta/lib", "org-b", false)
	})
	if err != nil {
		t.Fatal(err)
	}

	s := spoolForTest(t)
	s.Bind(account)
	if err := s.Append(
		spool.Event{
			Name: semconv.TermaToolCallEvent, Repository: config.Repository{Origin: "github.com/acme/app"},
			Profile: config.DefaultProfile, Enrollment: enr["team-a"],
		},
		spool.Event{
			Name: semconv.TermaToolCallEvent, Repository: config.Repository{Origin: "github.com/beta/lib"},
			Profile: config.DefaultProfile, Enrollment: enr["team-b"],
		},
		// Destination-less event: must drop and count as no_destination, exiting ExitIncomplete.
		spool.Event{
			Name: semconv.TermaCommitEvent, Repository: config.Repository{Origin: "github.com/acme/app"},
			Profile: config.DefaultProfile,
		},
	); err != nil {
		t.Fatal(err)
	}

	out, err := runTerma(t, "spool", "flush")
	if code, ok := exitCodeOf(err); !ok || code != ExitIncomplete {
		t.Fatalf("expected ExitIncomplete due to no_destination loss, got err=%v\n%s", err, out)
	}
	for _, want := range []string{"Flushed 2 events", "dropped 1 with no destination"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}

	mu.Lock()
	got := append([]reqSeen(nil), seen...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("sent %d requests, want 2", len(got))
	}
	var projects []string
	for _, r := range got {
		if r.auth != "Bearer "+fakeUserToken("u-1") {
			t.Errorf("auth = %q, want user token", r.auth)
		}
		if !strings.Contains(r.body, `"stringValue":"u-1"`) {
			t.Errorf("enrolledUser must stamp terma.user.id for all enrolled teams: %s", r.body)
		}
		projects = append(projects, r.project)
	}
	slices.Sort(projects)
	if !slices.Equal(projects, []string{"team-a", "team-b"}) {
		t.Fatalf("X-Mirador-Project headers = %v", projects)
	}

	losses, err := spool.ReadLosses(filepath.Join(testApp.stateDir, spool.Dir), time.Now())
	if err != nil || losses.Counts[delivery.LossNoDestination] != 1 {
		t.Fatalf("losses = %+v, err = %v", losses, err)
	}
}

func TestCLISpoolFlushRoutedServerKeyOmitsProjectHeader(t *testing.T) {
	useConfigDir(t, t.TempDir())
	t.Setenv("TERMA_AUTH_URL", "https://auth.example")
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) {
		p.ServerKeySignIn, p.ServerKeyAuthURL, p.Team = true, "https://auth.example", "team-srv"
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var gotAuth, gotProject, gotBody string
	ingest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotAuth, gotProject, gotBody = r.Header.Get("Authorization"), r.Header.Get(sendas.ProjectHeader), string(b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ingest.Close)
	t.Setenv("TERMA_OTLP_URL", ingest.URL)

	if err := keystore.Set(testApp.dir, "team-srv", testServerKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	var enrID string
	_, err := routing.Create(testApp.stateDir, config.DefaultProfile, sendas.NoPersonLane, func(s *routing.State) error {
		id, err := s.Select(routing.Team{
			OrganizationID: "org-s", AuthURL: "https://auth.example", TeamID: "team-srv",
			Policy: config.Policy{
				Mode: config.ModeRepo, TeamID: "team-srv", OrganizationID: "org-s", AuthURL: "https://auth.example",
				Repositories: []string{"github.com/acme/app"}, Revision: 1, FetchedAt: time.Now(),
				IncludePrompts: true, IncludeToolContent: true,
			},
		})
		if err != nil {
			return err
		}
		enrID = id
		return s.Bind("github.com/acme/app", "org-s", false)
	})
	if err != nil {
		t.Fatal(err)
	}

	s := spoolForTest(t)
	s.BindNoPerson()
	if err := s.Append(spool.Event{
		Name: semconv.TermaCommitEvent, Repository: config.Repository{Origin: "github.com/acme/app"},
		Profile: config.DefaultProfile, Enrollment: enrID,
	}); err != nil {
		t.Fatal(err)
	}

	out, err := runTerma(t, "spool", "flush")
	if err != nil {
		t.Fatalf("flush: %v\n%s", err, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer "+testServerKey {
		t.Fatalf("Authorization = %q, want server key", gotAuth)
	}
	if gotProject != "" {
		t.Fatalf("server-key routed flush must omit X-Mirador-Project, got %q", gotProject)
	}
	if strings.Contains(gotBody, semconv.TermaUserIDKey) {
		t.Fatalf("server-key routed flush must omit terma.user.id: %s", gotBody)
	}
}

func TestCLISpoolFlushRoutedPolicyDropsSucceed(t *testing.T) {
	f := newFakeAuth(t)
	authSandbox(t, f)
	if err := saveSignIn(testApp.dir, storedSession(f)); err != nil {
		t.Fatal(err)
	}
	account := auth.AccountKey(f.srv.URL, "u-1")
	acceptingOTLP(t)

	var enrID string
	_, err := routing.Create(testApp.stateDir, config.DefaultProfile, account, func(s *routing.State) error {
		id, err := s.Select(routing.Team{
			OrganizationID: "org-a", AuthURL: f.srv.URL, TeamID: "team-a",
			Policy: config.Policy{
				Mode: config.ModeRepo, TeamID: "team-a", OrganizationID: "org-a", AuthURL: f.srv.URL,
				Repositories: []string{"github.com/acme/app"}, Revision: 1, FetchedAt: time.Now(),
				IncludePrompts: true, IncludeToolContent: true,
			},
		})
		if err != nil {
			return err
		}
		enrID = id
		return s.Bind("github.com/acme/app", "org-a", false)
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	if _, err := runs.Start(testApp.stateDir, "sess-mixed", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := runs.MarkMixed(testApp.stateDir, "sess-mixed", now.Add(-30*time.Second), runs.Why{Reason: runs.Crossed}); err != nil {
		t.Fatal(err)
	}

	s := spoolForTest(t)
	s.Bind(account)
	if err := s.Append(
		spool.Event{
			Time: now, Name: semconv.TermaToolCallEvent, SessionID: "sess-mixed",
			Repository: config.Repository{Origin: "github.com/acme/app"},
			Profile:    config.DefaultProfile, Enrollment: enrID,
		},
		spool.Event{
			Time: now, Name: semconv.TermaToolCallEvent,
			Repository: config.Repository{Origin: "github.com/acme/app"},
			Profile:    config.DefaultProfile, Enrollment: "enr-ended",
		},
	); err != nil {
		t.Fatal(err)
	}

	out, err := runTerma(t, "spool", "flush")
	if err != nil {
		t.Fatalf("policy/Mixed drops must not fail flush: %v\n%s", err, out)
	}
	for _, want := range []string{"dropped 1 their team no longer collects", "dropped 1 because their session's run is Mixed (1 crossed organizations)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}
