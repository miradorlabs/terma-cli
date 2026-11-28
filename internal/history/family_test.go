package history

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// twoOrgState selects team-a (org-a, repo app) and team-b (org-b, repo secret), and a
// Personal team when personal is true.
func twoOrgState(now time.Time, personal bool) routing.State {
	team := func(eid, teamID, org string, pol config.Policy) routing.Team {
		pol.IncludePrompts, pol.IncludeToolContent = true, true
		pol.OrganizationID, pol.TeamID, pol.AuthURL, pol.FetchedAt = org, teamID, "https://auth.example.com", now
		return routing.Team{EnrollmentID: eid, TeamID: teamID, OrganizationID: org, AuthURL: "https://auth.example.com", Policy: pol}
	}
	st := routing.State{
		StateID: "st-1", Generation: 1, ConsentGeneration: 1, Account: "acc1",
		Teams: []routing.Team{
			team("enr-a", "team-a", "org-a", config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-a/app"}}),
			team("enr-b", "team-b", "org-b", config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-b/secret"}}),
		},
	}
	if personal {
		p := team("enr-p", "team-p", "org-p", config.Policy{Mode: config.ModeGlobal})
		p.Personal = true
		st.Teams = append(st.Teams, p)
	}
	return st
}

// captureServer records each request body by destination team.
func captureServer(t *testing.T) (*httptest.Server, func() map[string]string) {
	t.Helper()
	var mu sync.Mutex
	byTeam := map[string][]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		byTeam[r.Header.Get(sendas.ProjectHeader)] = append(byTeam[r.Header.Get(sendas.ProjectHeader)], string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, func() map[string]string {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]string{}
		for k, v := range byTeam {
			out[k] = strings.Join(v, "\n")
		}
		return out
	}
}

func uploadOpts(stateDir string, st routing.State, srv *httptest.Server, now time.Time, srcs ...agents.HistorySource) Options {
	return Options{
		StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: st, Sources: srcs, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
		Transport:   &Transport{Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"tok"}}, BytesPerSec: 1 << 30},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
	}
}

func checkSent(t *testing.T, got string, want, leaks []string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("expected %q to be uploaded", w)
		}
	}
	for _, l := range leaks {
		if strings.Contains(got, l) {
			t.Errorf("%q was uploaded", l)
		}
	}
}

// writeOldFile writes path with an mtime three hours before now.
func writeOldFile(t *testing.T, path string, now time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-3 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestWatchDirCompanionNotDiscoveredOrUnstatableSkipsSession(t *testing.T) {
	t.Parallel()
	repoA := initTestGitRepo(t, "https://github.com/org-a/app.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	t0 := now.Add(-4 * time.Hour)

	run := func(t *testing.T, withKnown bool, mutate func(watch string)) Summary {
		t.Helper()
		dir := t.TempDir()
		mainPath := filepath.Join(dir, "s-watch.jsonl")
		watch := filepath.Join(dir, "s-watch", "subagents")
		known := filepath.Join(watch, "agent-known.jsonl")
		writeOldFile(t, mainPath, now)
		var extras []string
		if withKnown {
			writeOldFile(t, known, now)
			extras = []string{known}
		} else if err := os.MkdirAll(watch, 0o755); err != nil {
			t.Fatal(err)
		}
		mutate(watch)
		src := &stubHistorySource{
			source: "claude",
			files: []agents.HistoryFile{{
				SourceSystem: "claude", SessionID: "s-watch", Path: mainPath, ExtraPaths: extras,
				WatchDirs: []string{watch}, ModTime: now.Add(-3 * time.Hour),
			}},
			segments: map[string][]agents.HistorySegment{"s-watch": {makeNativeSegment("claude", "s-watch", repoA, "WATCH_DIR_LEAK", t0)}},
		}
		srv, got := captureServer(t)
		sum, err := Upload(context.Background(), uploadOpts(t.TempDir(), twoOrgState(now, false), srv, now, src))
		if err != nil {
			t.Fatal(err)
		}
		checkSent(t, got()["team-a"], nil, []string{"WATCH_DIR_LEAK"})
		return sum
	}

	t.Run("appeared", func(t *testing.T) {
		sum := run(t, true, func(watch string) { writeOldFile(t, filepath.Join(watch, "agent-new.jsonl"), now) })
		if sum.SkippedSessions[SkipActive] != 1 {
			t.Errorf("skipped = %v, want active=1", sum.SkippedSessions)
		}
	})
	t.Run("unstatable", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a directory that lists but cannot be searched")
		}
		// No known companion, so the only entry that cannot be stat'ed is the listed one.
		sum := run(t, false, func(watch string) {
			writeOldFile(t, filepath.Join(watch, "agent-new.jsonl"), now)
			if err := os.Chmod(watch, 0o444); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(watch, 0o755) })
		})
		if sum.SkippedSessions[SkipUnreadable] != 1 {
			t.Errorf("skipped = %v, want unreadable=1", sum.SkippedSessions)
		}
	})
}

func TestHoldingsPageMismatchOrEmptyMiddlePageFailsClosed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	for name, body := range map[string]string{
		"page_mismatch":     `{"project_id":"team-a","sessions":[],"pagination":{"page":2,"per_page":200,"total":0,"total_pages":1}}`,
		"empty_middle_page": `{"project_id":"team-a","sessions":[],"pagination":{"page":1,"per_page":200,"total":300,"total_pages":2}}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			checker := &APIHoldingsChecker{
				ClientFor: func(teamID string) (*api.Client, error) {
					cfg := &config.Config{APIURL: srv.URL, AuthURL: "https://auth.example.com", Dir: t.TempDir(), ProfileName: "default"}
					return api.New(cfg, api.Options{Version: "test", ProjectID: teamID, Credential: newTestUserCred("https://auth.example.com", now)})
				},
			}
			if _, err := checker.Check(context.Background(), routing.Team{TeamID: "team-a"}, []string{"sess-1"}); err == nil {
				t.Fatal("expected fail-closed error, got nil")
			}
		})
	}
}

func TestDryRunLeavesOutSessionWithRecordOverRequestLimit(t *testing.T) {
	t.Parallel()
	repoA := initTestGitRepo(t, "https://github.com/org-a/app.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	src := &stubHistorySource{
		source:   "claude",
		files:    []agents.HistoryFile{{SourceSystem: "claude", SessionID: "s-big", ModTime: now.Add(-3 * time.Hour)}},
		segments: map[string][]agents.HistorySegment{"s-big": {makeNativeSegment("claude", "s-big", repoA, strings.Repeat("x", 4096), now.Add(-4*time.Hour))}},
	}
	srv, _ := captureServer(t)
	opts := uploadOpts(t.TempDir(), twoOrgState(now, false), srv, now, src)
	opts.DryRun, opts.MaxReqBytes = true, 1024
	sum, err := Upload(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Planned) != 0 || sum.SkippedSessions[SkipRecordTooLarge] != 1 {
		t.Errorf("planned = %+v, skipped = %v; want nothing planned and record_too_large=1", sum.Planned, sum.SkippedSessions)
	}
}

// refusedOverlapState selects team-a (org-a, Active, lists app and shared) and team-b
// (org-b, lists shared and secret, newer policy) with refusals as team-b's count.
func refusedOverlapState(now time.Time, refusals int) routing.State {
	st := twoOrgState(now, false)
	st.Teams[0].Policy.Repositories = []string{"github.com/org-a/app", "github.com/org-x/shared"}
	st.Teams[0].Policy.UpdatedAt = now.Add(-2 * time.Hour)
	st.Teams[1].Policy.Repositories = []string{"github.com/org-x/shared", "github.com/org-b/secret"}
	st.Teams[1].Policy.UpdatedAt = now.Add(-time.Hour)
	st.Teams[1].Refusals = refusals
	return st
}

// A Refused team is passed over, as the live hooks' routing does: what it lists goes to the
// next team that lists it, or nowhere. A Suspended team still holds what it lists.
func TestRefusedTeamIsPassedOverAndSuspendedTeamStillHolds(t *testing.T) {
	t.Parallel()
	repoA := initTestGitRepo(t, "https://github.com/org-a/app.git")
	shared := initTestGitRepo(t, "https://github.com/org-x/shared.git")
	repoB := initTestGitRepo(t, "https://github.com/org-b/secret.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	t0 := now.Add(-4 * time.Hour)
	src := &stubHistorySource{
		source: "claude",
		files: []agents.HistoryFile{
			{SourceSystem: "claude", SessionID: "s-app", ModTime: now.Add(-3 * time.Hour)},
			{SourceSystem: "claude", SessionID: "s-shared", ModTime: now.Add(-3 * time.Hour)},
			{SourceSystem: "claude", SessionID: "s-secret", ModTime: now.Add(-3 * time.Hour)},
		},
		segments: map[string][]agents.HistorySegment{
			"s-app":    {makeNativeSegment("claude", "s-app", repoA, "IN_APP", t0)},
			"s-shared": {makeNativeSegment("claude", "s-shared", shared, "IN_SHARED", t0)},
			"s-secret": {makeNativeSegment("claude", "s-secret", repoB, "IN_SECRET", t0)},
		},
	}

	srv, got := captureServer(t)
	sum, err := Upload(context.Background(), uploadOpts(t.TempDir(), refusedOverlapState(now, 2), srv, now, src))
	if err != nil {
		t.Fatal(err)
	}
	sent := got()
	checkSent(t, sent["team-a"], []string{"IN_APP", "IN_SHARED"}, []string{"IN_SECRET"})
	if sent["team-b"] != "" {
		t.Errorf("a Refused team was sent to: %q", sent["team-b"])
	}
	if sum.SkippedSegments[SkipNotCollected] != 1 || sum.SkippedSegments[SkipTeamBlockedPrefix+string(routing.Refused)] != 0 {
		t.Errorf("Refused team-b: want only s-secret not collected, got %+v", sum.SkippedSegments)
	}

	srv, got = captureServer(t)
	sum, err = Upload(context.Background(), uploadOpts(t.TempDir(), refusedOverlapState(now, 1), srv, now, src))
	if err != nil {
		t.Fatal(err)
	}
	sent = got()
	checkSent(t, sent["team-a"], []string{"IN_APP"}, []string{"IN_SHARED", "IN_SECRET"})
	if sent["team-b"] != "" {
		t.Errorf("a Suspended team was sent to: %q", sent["team-b"])
	}
	if sum.SkippedSegments[SkipTeamBlockedPrefix+string(routing.Suspended)] != 2 {
		t.Errorf("Suspended team-b: want s-shared and s-secret held, got %+v", sum.SkippedSegments)
	}
}

// Two Active organization teams that list the same repository: it goes to the one whose
// policy was updated most recently, whatever order the teams were selected in.
func TestRepositoryListedByTwoTeamsGoesToTheNewerPolicy(t *testing.T) {
	t.Parallel()
	shared := initTestGitRepo(t, "https://github.com/org-x/shared.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	for _, newer := range []int{0, 1} {
		st := twoOrgState(now, false)
		for i := range st.Teams {
			st.Teams[i].Policy.Repositories = append(st.Teams[i].Policy.Repositories, "github.com/org-x/shared")
			st.Teams[i].Policy.UpdatedAt = now.Add(-2 * time.Hour)
		}
		st.Teams[newer].Policy.UpdatedAt = now.Add(-time.Hour)
		r := newCwdResolver(st, "https://auth.example.com", false, "", "", now)
		target, ok, reason := r.resolve(context.Background(), shared, "", "")
		if !ok || target.Team.TeamID != st.Teams[newer].TeamID {
			t.Errorf("newer policy %s: routed to %q (%v, %q)", st.Teams[newer].TeamID, target.Team.TeamID, ok, reason)
		}
	}
}

// A new UpdatedAt can move a repository to another team, so it stops an upload's route; a
// refresh that only ticks FetchedAt and Generation does not.
func TestRoutingPoliciesMatchComparesUpdatedAtNotFetchedAt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	a := twoOrgState(now, false)
	for i := range a.Teams {
		a.Teams[i].Policy.UpdatedAt = now.Add(-time.Hour)
	}
	b := twoOrgState(now, false)
	b.Teams = slices.Clone(a.Teams)
	b.Generation++
	b.Teams[1].Policy.FetchedAt = now.Add(time.Minute)
	if !routingPoliciesMatch(a, b, now) {
		t.Error("a FetchedAt and Generation tick reads as a routing change")
	}
	b.Teams[1].Policy.UpdatedAt = now
	if routingPoliciesMatch(a, b, now) {
		t.Error("a new UpdatedAt on one team reads as no routing change")
	}
}
