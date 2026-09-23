package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hookrun"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

const spoolTestProject = "770e8400-e29b-41d4-a716-446655440000"

// spoolForTest opens the queue the command under test will use, so events can be
// seeded into it. TERMA_CONFIG_DIR must already point at a temp dir.
func spoolForTest(t *testing.T) *spool.Spool {
	t.Helper()
	dir, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	s, err := spool.Open(filepath.Join(dir, "spool"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// acceptingOTLP stands in for the ingest host: any request succeeds, so a test
// about accounting never fails for a delivery reason.
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
	e := spool.Event{Name: "terma.commit", Time: at}
	if projectID != "" {
		e.Attrs = map[string]any{hookrun.AttrProjectID: projectID}
	}
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
}

// An event with no project id can never be routed at all, while one that has aged
// out past the spool's MaxAge is time doing its work. Counting both as "gave up on
// N" left the developer with no way to tell which case they were looking at.
func TestFlushSpoolSeparatesUnroutableFromExpired(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	acceptingOTLP(t)
	s := spoolForTest(t)
	appendEvent(t, s, "", time.Now())
	appendEvent(t, s, spoolTestProject, time.Now().Add(-2*spool.MaxAge))

	res, err := flushSpool(context.Background(), true, 0)
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

// A fresh event for a project with no key is held, not lost: it stays queued, and
// the pass reports itself incomplete so a script knows to look again later.
func TestFlushSpoolHoldsEventsAndReportsIncomplete(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	acceptingOTLP(t)
	s := spoolForTest(t)
	appendEvent(t, s, "project-with-no-key-here", time.Now())

	out, err := runTerma(t, "spool", "flush")
	code, ok := exitCodeOf(err)
	if !ok || code != ExitIncomplete {
		t.Fatalf("held events should report incomplete: err=%v code=%d ok=%v\n%s", err, code, ok, out)
	}
	if !strings.Contains(out, "holding 1 for a project key") {
		t.Fatalf("the report should name what is held:\n%s", out)
	}
	if n, _, _ := s.Pending(); n != 1 {
		t.Fatalf("a held event must stay queued, pending=%d", n)
	}
}

// A flush that declines to run has done no work, which is not the same as a flush
// that ran and failed. It gets its own exit code, and --force overrides it.
func TestSpoolFlushReportsBackoffAsExitCode(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("TERMA_CONFIG_DIR", configDir)
	acceptingOTLP(t)
	// A failure an earlier flush recorded, still inside its retry window.
	spoolDir := filepath.Join(configDir, "spool")
	if err := os.MkdirAll(spoolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	next := time.Now().Add(time.Minute)
	attempt := strconv.FormatInt(next.Unix(), 10) + " 30"
	if err := os.WriteFile(filepath.Join(spoolDir, "next_attempt"), []byte(attempt), 0o600); err != nil {
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

// The throttle is the other reason a flush does nothing, and it is not a failure:
// a hook asks for a flush every turn, and "one just ran" is the answer it wants.
func TestSpoolFlushThrottleIsNotAnError(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	acceptingOTLP(t)
	if err := keystore.Set(spoolTestProject, "ter_srv_test"); err != nil {
		t.Fatal(err)
	}
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

// The happy path: every queued event reaches the backend under the project's key,
// and the command exits 0.
func TestSpoolFlushDeliversUnderTheProjectKey(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	auth := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TERMA_OTLP_URL", srv.URL)
	if err := keystore.Set(spoolTestProject, "ter_srv_test"); err != nil {
		t.Fatal(err)
	}
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

// `terma spool flush` and doctor word a pass from the same clauses, so a developer
// who reads "pruned" in one never reads a different word for it in the other. One
// clause per reason, in a fixed order, and a quiet pass has none.
func TestDescribeFlushNamesEveryReasonOnce(t *testing.T) {
	delivered, undelivered := describeFlush(flushResult{Sent: 1})
	if delivered != "1 event" || len(undelivered) != 0 {
		t.Fatalf("a clean pass = %q, %q", delivered, undelivered)
	}

	delivered, undelivered = describeFlush(flushResult{Sent: 2, Held: 3, Expired: 4, Pruned: 5, Unroutable: 6, Dropped: 7})
	if delivered != "2 events" {
		t.Errorf("delivered = %q", delivered)
	}
	want := []string{
		"holding 3 for a project key (run `terma install` in their repositories)",
		"expired 4 past the spool's age limit",
		"pruned 5 to stay under the size limit",
		"dropped 6 with no project id",
		"dropped 7 unreadable",
	}
	if strings.Join(undelivered, "|") != strings.Join(want, "|") {
		t.Errorf("undelivered =\n  %q\nwant\n  %q", undelivered, want)
	}
}
