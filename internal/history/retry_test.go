package history

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
)

// rejectingServer answers 400 to every request whose body does not contain accept.
func rejectingServer(t *testing.T, accept string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if accept != "" && strings.Contains(string(body), accept) {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"INVALID","message":"bad schema"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// numberedSessions returns n sessions in repo, s-01 first, each with one prompt naming it.
func numberedSessions(repo string, n int, now time.Time) *stubHistorySource {
	src := &stubHistorySource{source: "claude", segments: map[string][]agents.HistorySegment{}}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("s-%02d", i)
		at := now.Add(-48*time.Hour + time.Duration(i)*time.Minute)
		src.files = append(src.files, agents.HistoryFile{SourceSystem: "claude", SessionID: id, ModTime: at})
		src.segments[id] = []agents.HistorySegment{makeNativeSegment("claude", id, repo, "PROMPT_"+id, at)}
	}
	return src
}

func newlyFailed(sum Summary) []string {
	var ids []string
	for _, f := range sum.Failed {
		if !f.Previously {
			ids = append(ids, f.SessionID)
		}
	}
	return ids
}

// After five sessions in a row that the server rejects, the route stops with a team issue
// that names --retry-failed, and the sessions after them are not marked failed.
func TestRouteStopsAfterConsecutiveRejections(t *testing.T) {
	t.Parallel()
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	stateDir := t.TempDir()
	srv := rejectingServer(t, "")
	opts := uploadOpts(stateDir, singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true), srv, now, numberedSessions(repoA, 8, now))
	sum, err := Upload(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := newlyFailed(sum); len(got) != maxConsecutiveRejections {
		t.Errorf("failed sessions = %v, want %d", got, maxConsecutiveRejections)
	}
	if len(sum.TeamIssues) != 1 || !strings.Contains(sum.TeamIssues[0].Reason, "--retry-failed") || !strings.Contains(sum.TeamIssues[0].Reason, "5 sessions in a row") || sum.TeamIssues[0].Skipped != 3 {
		t.Errorf("team issues = %+v, want one naming 5 rejections and --retry-failed, 3 skipped", sum.TeamIssues)
	}
	st, _, err := LoadState(stateDir, "acc1", "default", "enr-team-a", "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Failed) != maxConsecutiveRejections {
		t.Errorf("state holds %d failed sessions, want %d", len(st.Failed), maxConsecutiveRejections)
	}
}

// A session the server accepts resets the count of rejections in a row.
func TestAcceptedSessionResetsRejectionCount(t *testing.T) {
	t.Parallel()
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	srv := rejectingServer(t, "PROMPT_s-03")
	opts := uploadOpts(t.TempDir(), singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true), srv, now, numberedSessions(repoA, 9, now))
	sum, err := Upload(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	// s-01, s-02 fail; s-03 is accepted; s-04 to s-08 fail and stop the route before s-09.
	if got := newlyFailed(sum); len(got) != 7 || sum.SentSessions != 1 {
		t.Errorf("failed %v, sent %d; want 7 failed and 1 sent", got, sum.SentSessions)
	}
	if len(sum.TeamIssues) != 1 || sum.TeamIssues[0].Skipped != 1 {
		t.Errorf("team issues = %+v, want one with s-09 skipped", sum.TeamIssues)
	}
}

// A session that failed in an earlier run is skipped without --retry-failed, and sent and
// marked done with it; dry run plans it only with the flag.
func TestRetryFailedSendsPreviouslyFailedSession(t *testing.T) {
	t.Parallel()
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	stateDir := t.TempDir()
	srv, got := captureServer(t)
	st := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	rs, _, err := LoadState(stateDir, "acc1", "default", "enr-team-a", "team-a")
	if err != nil {
		t.Fatal(err)
	}
	rs.MarkFailed("claude", "s-01", now.Add(-time.Hour), "HTTP 400: bad schema", 0)
	if err := SaveState(stateDir, rs); err != nil {
		t.Fatal(err)
	}
	run := func(retry, dryRun bool) Summary {
		t.Helper()
		opts := uploadOpts(stateDir, st, srv, now, numberedSessions(repoA, 1, now))
		opts.RetryFailed, opts.DryRun = retry, dryRun
		sum, err := Upload(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}
	if sum := run(false, true); len(sum.Planned) != 0 {
		t.Errorf("dry run without --retry-failed planned %+v", sum.Planned)
	}
	if sum := run(true, true); len(sum.Planned) != 1 || sum.Planned[0].Sessions != 1 || sum.Planned[0].EstimatedBytes == 0 {
		t.Errorf("dry run with --retry-failed planned %+v, want the failed session", sum.Planned)
	}
	if sum := run(false, false); sum.SentSessions != 0 || sum.SkippedSessions[SkipFailedPreviously] != 1 {
		t.Errorf("without --retry-failed: sent %d, skipped %v", sum.SentSessions, sum.SkippedSessions)
	}
	if sum := run(true, false); sum.SentSessions != 1 || len(sum.Failed) != 0 {
		t.Errorf("with --retry-failed: sent %d, failed %+v; want 1 sent", sum.SentSessions, sum.Failed)
	}
	if !strings.Contains(got()["team-a"], "PROMPT_s-01") {
		t.Errorf("the retried session was not uploaded")
	}
	rs, _, err = LoadState(stateDir, "acc1", "default", "enr-team-a", "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, failed := rs.IsFailed(SessionKey("claude", "s-01")); failed || !rs.IsDone(SessionKey("claude", "s-01")) {
		t.Errorf("state after retry: failed=%v done=%v, want done only", failed, rs.IsDone(SessionKey("claude", "s-01")))
	}
}
