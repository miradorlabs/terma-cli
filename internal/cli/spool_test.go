package cli

import (
	"cmp"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/delivery"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

const spoolTestProject = "770e8400-e29b-41d4-a716-446655440000"

// spoolForTest opens the queue the command will use, under testApp's state directory, on
// a machine set up.
func spoolForTest(t *testing.T) *spool.Spool {
	t.Helper()
	hookruntest.RelayOn(t, testApp.stateDir)
	s, err := spool.Open(filepath.Join(testApp.stateDir, spool.Dir))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// acceptingOTLP stands in for an ingest host that accepts every request.
func acceptingOTLP(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TERMA_OTLP_URL", srv.URL)
}

func appendEvent(t *testing.T, s *spool.Spool, projectID string, at time.Time) {
	t.Helper()
	// Bound to the sign-in in place, or no person's under a server key, as a hook's append
	// is; signed out, to a person since signed out.
	e := spool.Event{Name: "terma.commit", Time: at, Repository: appRepo, Account: "aaaaaaaaaaaaaaaa"}
	if cfg, err := testApp.loadConfig(); err == nil && cfg.ServerKeySignIn {
		e.Account, e.NoPerson = "", true
	} else if err == nil {
		e.Account = cmp.Or(auth.Account(cfg.Dir, cfg.ProfileName, cfg.AuthURL), e.Account)
	}
	if projectID != "" {
		e.Attrs = map[string]any{hookrun.AttrProjectID: projectID}
		if profile, tm, ok := findEnrolledTeam(projectID); ok {
			e.Profile, e.Enrollment = profile, tm.EnrollmentID
			if len(tm.Policy.Repositories) > 0 {
				e.Repository = config.Repository{Origin: tm.Policy.Repositories[0]}
			} else if tm.Personal {
				e.Repository = config.Repository{Origin: "github.com/personal/side"}
			}
		}
	}
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
}

func findEnrolledTeam(projectID string) (string, routing.Team, bool) {
	check := func(profile string) (routing.Team, bool) {
		st, found, err := routing.Load(testApp.stateDir, profile)
		if err != nil || !found {
			return routing.Team{}, false
		}
		for _, tm := range st.Teams {
			if tm.TeamID == projectID {
				return tm, true
			}
		}
		return routing.Team{}, false
	}
	if tm, ok := check(config.DefaultProfile); ok {
		return config.DefaultProfile, tm, true
	}
	if file, err := config.LoadFile(testApp.dir); err == nil {
		for name := range file.Profiles {
			if name == "" || name == config.DefaultProfile {
				continue
			}
			if tm, ok := check(name); ok {
				return name, tm, true
			}
		}
	}
	return "", routing.Team{}, false
}

// An event with no destination enrollment is counted as unroutable, apart from one that aged out.
func TestFlushSpoolSeparatesUnroutableFromExpired(t *testing.T) {
	useConfigDir(t, t.TempDir())
	acceptingOTLP(t)
	acct := signInHereAs(t, "u-spool")
	s := spoolForTest(t)
	appendEvent(t, s, "", time.Now())
	if err := s.Append(spool.Event{Name: "terma.commit", Time: time.Now().Add(-2 * spool.MaxAge), Repository: appRepo, Account: acct,
		Attrs: map[string]any{hookrun.AttrProjectID: spoolTestProject}}); err != nil {
		t.Fatal(err)
	}

	res, err := testApp.flushSpool(context.Background(), true, 0)
	if err != nil {
		t.Fatalf("flushSpool: %v", err)
	}
	if res.Unroutable != 1 || res.Expired != 1 {
		t.Fatalf("want one of each, got %+v", res)
	}
	if res.Sent != 0 || res.Held != 0 || res.Dropped != 0 || res.Pruned != 0 {
		t.Fatalf("nothing else should be counted: %+v", res)
	}
	if !res.Lost() {
		t.Error("both kinds of discard are loss")
	}
}

// An event for a project with no key is held, and the pass reports itself incomplete.
func TestFlushSpoolHoldsEventsAndReportsIncomplete(t *testing.T) {
	useConfigDir(t, t.TempDir())
	acceptingOTLP(t)
	s := spoolForTest(t)
	appendEvent(t, s, "project-with-no-key-here", time.Now())

	out, err := runTerma(t, "spool", "flush")
	code, ok := exitCodeOf(err)
	if !ok || code != ExitIncomplete {
		t.Fatalf("held events should report incomplete: err=%v code=%d ok=%v\n%s", err, code, ok, out)
	}
	if !strings.Contains(out, "holding 1 for a sign-in that may send them") {
		t.Fatalf("the report should name what is held:\n%s", out)
	}
	if n, _, _ := s.Pending(); n != 1 {
		t.Fatalf("a held event must stay queued, pending=%d", n)
	}
}

// A flush declined by its retry window has its own exit code, and --force overrides it.
func TestSpoolFlushReportsBackoffAsExitCode(t *testing.T) {
	useConfigDir(t, t.TempDir())
	hookruntest.RelayOn(t, testApp.stateDir)
	acceptingOTLP(t)
	spoolDir := filepath.Join(testApp.stateDir, spool.Dir)
	if err := os.MkdirAll(spoolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	next := time.Now().Add(time.Minute)
	attempt := strconv.FormatInt(next.Unix(), 10) + " 30"
	if err := os.WriteFile(filepath.Join(spoolDir, "next-attempt-v2"), []byte(attempt), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runTerma(t, "spool", "flush")
	code, ok := exitCodeOf(err)
	if !ok || code != ExitBackoff {
		t.Fatalf("a backoff window should report its own code: err=%v code=%d ok=%v\n%s", err, code, ok, out)
	}
	if !strings.Contains(out, "Skipped: backing off") {
		t.Fatalf("the report should explain the window:\n%s", out)
	}

	if out, err := runTerma(t, "spool", "flush", "--force"); err != nil {
		t.Fatalf("--force should override the window: %v\n%s", err, out)
	}
}

// The throttle is not a failure: a hook asks for a flush every turn.
func TestSpoolFlushThrottleIsNotAnError(t *testing.T) {
	useConfigDir(t, t.TempDir())
	acceptingOTLP(t)
	serverKeyProfile(t)
	routeProject(t, spoolTestProject, "ter_srv_test", "")
	s := spoolForTest(t)
	appendEvent(t, s, spoolTestProject, time.Now())

	out, err := runTerma(t, "spool", "flush", "--min-interval", "10m")
	if err != nil {
		t.Fatalf("the first flush should run: %v\n%s", err, out)
	}
	out, err = runTerma(t, "spool", "flush", "--min-interval", "10m")
	if err != nil {
		t.Fatalf("a throttled flush should still succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Skipped: a flush ran recently") {
		t.Fatalf("the report should name the throttle:\n%s", out)
	}
}

// On a profile set up with a server key, every queued event reaches the backend under the
// project's key, and the command exits 0.
func TestSpoolFlushDeliversUnderTheProjectKey(t *testing.T) {
	useConfigDir(t, t.TempDir())
	auth := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TERMA_OTLP_URL", srv.URL)
	serverKeyProfile(t)
	routeProject(t, spoolTestProject, "ter_srv_test", "")
	s := spoolForTest(t)
	appendEvent(t, s, spoolTestProject, time.Now())
	appendEvent(t, s, spoolTestProject, time.Now())

	out, err := runTerma(t, "spool", "flush")
	if err != nil {
		t.Fatalf("flush: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Flushed 2 events") {
		t.Fatalf("unexpected report:\n%s", out)
	}
	if got := <-auth; got != "Bearer ter_srv_test" {
		t.Fatalf("unexpected Authorization header %q", got)
	}
	if n, _, _ := s.Pending(); n != 0 {
		t.Fatalf("delivered events must leave the queue, pending=%d", n)
	}
}

// describeFlush gives one clause per reason, in a fixed order, and none for a quiet pass.
func TestDescribeFlushNamesEveryReasonOnce(t *testing.T) {
	delivered, undelivered := describeFlush(delivery.Result{Sent: 1}, "terma setup")
	if delivered != "1 event" || len(undelivered) != 0 {
		t.Fatalf("a clean pass = %q, %q", delivered, undelivered)
	}

	delivered, undelivered = describeFlush(delivery.Result{Sent: 2, Held: 3, Expired: 4, Pruned: 5, Unroutable: 6, Dropped: 7}, "terma setup")
	if delivered != "2 events" {
		t.Errorf("delivered = %q", delivered)
	}
	want := []string{
		"holding 3 for a sign-in that may send them (run `terma setup`)",
		"expired 4 past the spool's age limit",
		"pruned 5 to stay under the size limit",
		"dropped 6 with no team id",
		"dropped 7 unreadable",
	}
	if strings.Join(undelivered, "|") != strings.Join(want, "|") {
		t.Errorf("undelivered =\n  %q\nwant\n  %q", undelivered, want)
	}
	// A retired sign-in's fix quotes its own commands: never twice.
	if _, undelivered = describeFlush(delivery.Result{Held: 1}, doctor.RetiredFix); undelivered[0] != "holding 1 for a sign-in that may send them (run `terma update`, then `terma setup`)" {
		t.Errorf("retired = %q", undelivered[0])
	}
}
