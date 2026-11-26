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
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/runs"
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

// A missing path under a working directory outside git is not that directory's own: it
// may be an organization repository since deleted, so it cuts the session.
func TestMissingPathUnderNonGitCwdIsUnknown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	t0 := now.Add(-4 * time.Hour)
	before := makeNativeSegment("claude", "s-home", cwd, "BEFORE_READ_OK", t0)
	after := makeNativeSegment("claude", "s-home", cwd, "AFTER_DELETED_REPO_READ", t0.Add(2*time.Minute))
	after.ToolReaches = []agents.HistoryToolReach{{At: t0.Add(time.Minute), Cwd: cwd, Paths: []string{filepath.Join(cwd, "deleted-orgb-repo", "secret.go")}}}
	src := &stubHistorySource{
		source:   "claude",
		files:    []agents.HistoryFile{{SourceSystem: "claude", SessionID: "s-home", ModTime: now.Add(-3 * time.Hour)}},
		segments: map[string][]agents.HistorySegment{"s-home": {before, after}},
	}
	srv, got := captureServer(t)
	if _, err := Upload(context.Background(), uploadOpts(t.TempDir(), twoOrgState(now, true), srv, now, src)); err != nil {
		t.Fatal(err)
	}
	checkSent(t, got()["team-p"], []string{"BEFORE_READ_OK"}, []string{"AFTER_DELETED_REPO_READ"})
}

// A segment whose checkout is since deleted is placed by the origin its transcript recorded,
// and what it reached under its own working directory does not cut it. A deleted path
// anywhere else still does.
func TestReachUnderDeletedRecordedCheckoutIsItsOwn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	gone := filepath.Join(home, "worktrees", "app-1")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	t0 := now.Add(-4 * time.Hour)
	seg := func(text string, at time.Time, reach string) agents.HistorySegment {
		s := makeNativeSegment("codex", "s-gone", gone, text, at)
		s.FallbackRepoURL = "https://github.com/org-a/app.git"
		s.ToolReaches = []agents.HistoryToolReach{{At: at.Add(-time.Second), Cwd: gone, Paths: []string{reach}}}
		return s
	}
	src := &stubHistorySource{
		source: "codex",
		files:  []agents.HistoryFile{{SourceSystem: "codex", SessionID: "s-gone", ModTime: now.Add(-3 * time.Hour)}},
		segments: map[string][]agents.HistorySegment{"s-gone": {
			seg("AFTER_OWN_FILE_READ", t0, filepath.Join(gone, "internal", "main.go")),
			seg("AFTER_OTHER_DELETED_READ", t0.Add(time.Minute), filepath.Join(home, "worktrees", "secret-1", "keys.go")),
		}},
	}
	srv, got := captureServer(t)
	if _, err := Upload(context.Background(), uploadOpts(t.TempDir(), twoOrgState(now, true), srv, now, src)); err != nil {
		t.Fatal(err)
	}
	checkSent(t, got()["team-a"], []string{"AFTER_OWN_FILE_READ"}, []string{"AFTER_OTHER_DELETED_READ"})
}

// A family with subagents must sit in one organization: once the parent's transcript moves
// to org B while its child still works in org A, nothing from then on uploads.
func TestFamilyWithSubagentsSpanningTwoOrgsIsCutWhenSecondAppears(t *testing.T) {
	t.Parallel()
	repoA := initTestGitRepo(t, "https://github.com/org-a/app.git")
	repoB := initTestGitRepo(t, "https://github.com/org-b/secret.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	t0 := now.Add(-4 * time.Hour)
	src := &stubHistorySource{
		source: "codex",
		files: []agents.HistoryFile{
			{SourceSystem: "codex", SessionID: "s-par", ModTime: now.Add(-3 * time.Hour)},
			{SourceSystem: "codex", SessionID: "s-kid", ParentSessionID: "s-par", ModTime: now.Add(-3 * time.Hour)},
		},
		segments: map[string][]agents.HistorySegment{
			"s-par": {
				makeNativeSegment("codex", "s-par", repoA, "PARENT_IN_A_OK", t0),
				makeNativeSegment("codex", "s-par", repoB, "PARENT_IN_B_LEAK", t0.Add(10*time.Minute)),
			},
			"s-kid": {
				makeNativeSegment("codex", "s-kid", repoA, "CHILD_EARLY_A_OK", t0.Add(time.Minute)),
				makeNativeSegment("codex", "s-kid", repoA, "CHILD_LATE_A_LEAK", t0.Add(20*time.Minute)),
			},
		},
	}
	srv, got := captureServer(t)
	if _, err := Upload(context.Background(), uploadOpts(t.TempDir(), twoOrgState(now, false), srv, now, src)); err != nil {
		t.Fatal(err)
	}
	sent := got()
	checkSent(t, sent["team-a"]+"\n"+sent["team-b"], []string{"PARENT_IN_A_OK", "CHILD_EARLY_A_OK"}, []string{"PARENT_IN_B_LEAK", "CHILD_LATE_A_LEAK"})
}

// A reach to a top-level directory that no longer exists, or to a path under it, cuts the
// session at that call, rather than reading as a reach to "/".
func TestMissingTopLevelPathCutsSession(t *testing.T) {
	t.Parallel()
	repoA := initTestGitRepo(t, "https://github.com/org-a/app.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	t0 := now.Add(-4 * time.Hour)
	gone := "/terma-nonexistent-mount-" + filepath.Base(t.TempDir())
	shell := func(cmd string) []string { return hookrun.Alone(hookrun.ShellPathsHistorical(cmd, repoA)...) }
	for name, paths := range map[string][]string{
		"file under it":    shell("cat " + gone + "/org-b/notes"),
		"search of it":     shell("rg apiKey " + gone),
		"cd into it":       shell("cd " + gone + " && cat secret.go"),
		"search tool path": hookrun.SearchHistorical(gone, "*.go"),
	} {
		before := makeNativeSegment("claude", "s-mount", repoA, "BEFORE_MOUNT_OK", t0)
		after := makeNativeSegment("claude", "s-mount", repoA, "AFTER_MOUNT_LEAK", t0.Add(2*time.Minute))
		after.ToolReaches = []agents.HistoryToolReach{{At: t0.Add(time.Minute), Cwd: repoA, Paths: paths}}
		src := &stubHistorySource{
			source:   "claude",
			files:    []agents.HistoryFile{{SourceSystem: "claude", SessionID: "s-mount", ModTime: now.Add(-3 * time.Hour)}},
			segments: map[string][]agents.HistorySegment{"s-mount": {before, after}},
		}
		srv, got := captureServer(t)
		sum, err := Upload(context.Background(), uploadOpts(t.TempDir(), twoOrgState(now, false), srv, now, src))
		if err != nil {
			t.Fatal(err)
		}
		if sent := got()["team-a"]; !strings.Contains(sent, "BEFORE_MOUNT_OK") || strings.Contains(sent, "AFTER_MOUNT_LEAK") {
			t.Errorf("%s (%q): want the session cut at the reach", name, paths)
		}
		if sum.SentSessions != 1 || sum.WithheldRecords != 1 {
			t.Errorf("%s: sent %d session(s), withheld %d record(s); want 1 and 1", name, sum.SentSessions, sum.WithheldRecords)
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

// A live hook can mark a family member's run Mixed between two members' uploads; the
// transcript verdict is cached per family, but the ledgers are read every time.
func TestCompanionLedgerReadForEveryMember(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	repoA := initTestGitRepo(t, "https://github.com/org-a/app.git")
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	t0 := now.Add(-4 * time.Hour)
	for _, id := range []string{"s-root", "s-kid1", "s-kid2"} {
		if _, err := runs.Start(stateDir, id, t0.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	src := &stubHistorySource{
		source: "codex",
		files: []agents.HistoryFile{
			{SourceSystem: "codex", SessionID: "s-root", ModTime: now.Add(-3 * time.Hour)},
			{SourceSystem: "codex", SessionID: "s-kid1", ParentSessionID: "s-root", ModTime: now.Add(-3*time.Hour + time.Minute)},
			{SourceSystem: "codex", SessionID: "s-kid2", ParentSessionID: "s-root", ModTime: now.Add(-3*time.Hour + 2*time.Minute)},
		},
		segments: map[string][]agents.HistorySegment{
			"s-root": {makeNativeSegment("codex", "s-root", repoA, "ROOT_OK", t0)},
			"s-kid1": {makeNativeSegment("codex", "s-kid1", repoA, "KID1_AFTER_MARK_LEAK", t0.Add(time.Minute))},
			"s-kid2": {makeNativeSegment("codex", "s-kid2", repoA, "KID2_AFTER_MARK_LEAK", t0.Add(time.Minute))},
		},
	}
	srv, got := captureServer(t)
	opts := uploadOpts(stateDir, twoOrgState(now, false), srv, now, src)
	var markErr error
	opts.Progress = func(ev ProgressEvent) {
		if ev.Kind == ProgressSessionDone && ev.SessionID == "s-root" {
			markErr = runs.MarkMixed(stateDir, "s-kid2", t0, runs.Why{Reason: runs.Crossed})
		}
	}
	if _, err := Upload(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if markErr != nil {
		t.Fatal(markErr)
	}
	checkSent(t, got()["team-a"], []string{"ROOT_OK"}, []string{"KID1_AFTER_MARK_LEAK", "KID2_AFTER_MARK_LEAK"})
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
// next team that lists it, or nowhere, and reaching it crosses into no organization. A
// Suspended team still holds what it lists, so the same reach cuts the session.
func TestRefusedTeamIsPassedOverAndSuspendedTeamStillHolds(t *testing.T) {
	t.Parallel()
	repoA := initTestGitRepo(t, "https://github.com/org-a/app.git")
	shared := initTestGitRepo(t, "https://github.com/org-x/shared.git")
	repoB := initTestGitRepo(t, "https://github.com/org-b/secret.git")
	secretB := filepath.Join(repoB, "secret.go")
	if err := os.WriteFile(secretB, []byte("package secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	t0 := now.Add(-4 * time.Hour)
	reach := makeNativeSegment("claude", "s-reach", repoA, "AFTER_REACH_INTO_B", t0.Add(2*time.Minute))
	reach.ToolReaches = []agents.HistoryToolReach{{At: t0.Add(time.Minute), Cwd: repoA, Paths: []string{secretB}}}
	src := &stubHistorySource{
		source: "claude",
		files: []agents.HistoryFile{
			{SourceSystem: "claude", SessionID: "s-reach", ModTime: now.Add(-3 * time.Hour)},
			{SourceSystem: "claude", SessionID: "s-shared", ModTime: now.Add(-3 * time.Hour)},
			{SourceSystem: "claude", SessionID: "s-secret", ModTime: now.Add(-3 * time.Hour)},
		},
		segments: map[string][]agents.HistorySegment{
			"s-reach":  {makeNativeSegment("claude", "s-reach", repoA, "BEFORE_REACH_OK", t0), reach},
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
	checkSent(t, sent["team-a"], []string{"BEFORE_REACH_OK", "AFTER_REACH_INTO_B", "IN_SHARED"}, []string{"IN_SECRET"})
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
	checkSent(t, sent["team-a"], []string{"BEFORE_REACH_OK"}, []string{"AFTER_REACH_INTO_B", "IN_SHARED", "IN_SECRET"})
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
