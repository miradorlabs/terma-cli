package history

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/api"
	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/routing"
)

// checkPlanAccounting checks the promise the progress events make: one plan, before any
// session starts, and every session it counts ends in exactly one done, failed or skipped
// event of its own, or in a later team_blocked count.
func checkPlanAccounting(t *testing.T, events []ProgressEvent) *UploadPlan {
	t.Helper()
	var plan *UploadPlan
	ended := map[int]int{}
	blockedAfter := 0
	for _, ev := range events {
		switch ev.Kind {
		case ProgressPlan:
			if plan != nil {
				t.Fatal("two plan events")
			}
			plan = ev.Plan
		case ProgressSessionStart, ProgressSessionDone, ProgressSessionFailed, ProgressSessionSkipped:
			if plan == nil {
				t.Fatalf("%s for %s before the plan", ev.Kind, ev.SessionID)
			}
			if ev.Overall < 1 || ev.Overall > plan.Sessions || ev.OverallTotal != plan.Sessions {
				t.Errorf("%s for %s numbered %d of %d, plan has %d", ev.Kind, ev.SessionID, ev.Overall, ev.OverallTotal, plan.Sessions)
			}
			if ev.Kind != ProgressSessionStart {
				ended[ev.Overall]++
			}
		case ProgressRouteBlocked:
			if plan != nil {
				blockedAfter += ev.Skipped
			}
		}
	}
	if plan == nil {
		t.Fatal("no plan event")
	}
	for i, n := range ended {
		if n != 1 {
			t.Errorf("session %d ended %d times", i, n)
		}
	}
	if got := len(ended) + blockedAfter; got != plan.Sessions {
		t.Errorf("%d sessions ended and %d were blocked, plan counted %d", len(ended), blockedAfter, plan.Sessions)
	}
	return plan
}

// A team that cannot be sent to when it is planned stops whole, with its resumed sessions,
// in one issue and before anything is sent; another team of an organization that refused
// access stops when its turn comes, without its server being asked.
func TestPlanStopsATeamWholeWhenItCannotBeSent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	repoB := initTestGitRepo(t, "https://github.com/org-a/repo-b.git")
	team := func(id, repo string) routing.Team {
		return routing.Team{
			EnrollmentID: "enr-" + id, TeamID: id, OrganizationID: "org-a", AuthURL: "https://auth.example.com",
			Policy: config.Policy{
				Mode: config.ModeRepo, Repositories: []string{repo}, IncludePrompts: true, IncludeToolContent: true,
				OrganizationID: "org-a", TeamID: id, AuthURL: "https://auth.example.com", FetchedAt: now,
			},
		}
	}
	st := routing.State{Account: "acc1", Teams: []routing.Team{team("team-a", "github.com/org-a/repo-a"), team("team-b", "github.com/org-a/repo-b")}}
	src := &stubHistorySource{
		source: "claude",
		files: []agents.HistoryFile{
			{SourceSystem: "claude", SessionID: "s-resume", ModTime: now.Add(-5 * time.Hour)},
			{SourceSystem: "claude", SessionID: "s-new-1", ModTime: now.Add(-4 * time.Hour)},
			{SourceSystem: "claude", SessionID: "s-new-2", ModTime: now.Add(-3 * time.Hour)},
			{SourceSystem: "claude", SessionID: "s-b", ModTime: now.Add(-3 * time.Hour)},
		},
		segments: map[string][]agents.HistorySegment{
			"s-resume": {makeNativeSegment("claude", "s-resume", repoA, "r", now.Add(-5*time.Hour))},
			"s-new-1":  {makeNativeSegment("claude", "s-new-1", repoA, "n1", now.Add(-4*time.Hour))},
			"s-new-2":  {makeNativeSegment("claude", "s-new-2", repoA, "n2", now.Add(-3*time.Hour))},
			"s-b":      {makeNativeSegment("claude", "s-b", repoB, "b", now.Add(-3*time.Hour))},
		},
	}
	run := func(t *testing.T, holdings HoldingsChecker, reload func() (routing.State, error)) (Summary, []ProgressEvent, int64) {
		t.Helper()
		stateDir := t.TempDir()
		rs := NewRouteState("acc1", "default", "enr-team-a", "team-a")
		rs.MarkInProgress("claude", "s-resume", now.Add(-time.Hour), 1, 100, time.Time{})
		if err := SaveState(stateDir, rs); err != nil {
			t.Fatal(err)
		}
		var sends atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			sends.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)
		var events []ProgressEvent
		sum, err := Upload(context.Background(), Options{
			StateDir: stateDir, Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
			State: st, ReloadState: reload, Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil), Holdings: holdings,
			Transport:   &Transport{Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"t1"}}, BytesPerSec: 1 << 30},
			EndpointFor: func(string) string { return srv.URL },
			Now:         func() time.Time { return now },
			Progress:    func(ev ProgressEvent) { events = append(events, ev) },
		})
		if err != nil {
			t.Fatal(err)
		}
		return sum, events, sends.Load()
	}

	t.Run("recheck fails", func(t *testing.T) {
		t.Parallel()
		calls := 0
		// The first reload is team-a's plan-time recheck; every later one succeeds.
		reload := func() (routing.State, error) {
			if calls++; calls == 1 {
				return routing.State{}, errors.New("team selections changed")
			}
			return st, nil
		}
		sum, events, _ := run(t, stubHoldings{}, reload)
		plan := checkPlanAccounting(t, events)
		if len(sum.TeamIssues) != 1 || sum.TeamIssues[0].TeamID != "team-a" || sum.TeamIssues[0].Skipped != 3 || sum.SkippedSessions[SkipDeselected] != 3 {
			t.Fatalf("issues = %+v skipped = %v, want one team-a issue of all 3 sessions", sum.TeamIssues, sum.SkippedSessions)
		}
		if plan.Sessions != 1 || sum.SentSessions != 1 {
			t.Errorf("plan = %d sessions, sent %d; want team-b's one", plan.Sessions, sum.SentSessions)
		}
	})

	t.Run("organization not accessible", func(t *testing.T) {
		t.Parallel()
		lost := &api.Error{StatusCode: http.StatusForbidden, Reason: api.ReasonOrgNotAccessible, Message: "organization is not accessible"}
		var asked []string
		holdings := checkerFunc(func(team routing.Team) error {
			asked = append(asked, team.TeamID)
			return lost
		})
		sum, events, sends := run(t, holdings, nil)
		plan := checkPlanAccounting(t, events)
		if len(asked) != 1 || asked[0] != "team-a" {
			t.Errorf("holdings asked of %v, want team-a only", asked)
		}
		if sends != 0 || plan.Sessions != 0 {
			t.Errorf("sent %d requests and planned %d sessions, want none", sends, plan.Sessions)
		}
		if len(sum.TeamIssues) != 2 || sum.TeamIssues[0].Skipped != 3 || sum.TeamIssues[1].TeamID != "team-b" || sum.SkippedSessions[SkipOrgAccessLost] != 4 {
			t.Fatalf("issues = %+v skipped = %v, want team-a's 3 and team-b's 1 as org_access_lost", sum.TeamIssues, sum.SkippedSessions)
		}
	})

	// team-b's holdings check finds the organization refusing access after team-a's passed:
	// the whole organization is blocked before the plan is emitted, so neither team is in the
	// plan and nothing is attempted.
	t.Run("organization refuses a later team", func(t *testing.T) {
		t.Parallel()
		lost := &api.Error{StatusCode: http.StatusForbidden, Reason: api.ReasonOrgNotAccessible, Message: "organization is not accessible"}
		holdings := checkerFunc(func(team routing.Team) error {
			if team.TeamID == "team-b" {
				return lost
			}
			return nil
		})
		sum, events, sends := run(t, holdings, nil)
		plan := checkPlanAccounting(t, events)
		if plan.Sessions != 0 || plan.SourceBytes != 0 || sends != 0 {
			t.Errorf("planned %d sessions (%d source bytes) and sent %d requests, want none", plan.Sessions, plan.SourceBytes, sends)
		}
		if len(sum.TeamIssues) != 2 || sum.TeamIssues[0].TeamID != "team-a" || sum.TeamIssues[0].Skipped != 3 ||
			sum.TeamIssues[1].TeamID != "team-b" || sum.TeamIssues[1].Skipped != 1 {
			t.Fatalf("issues = %+v, want team-a (3) and team-b (1) in route order", sum.TeamIssues)
		}
		if sum.SkippedSessions[SkipOrgAccessLost] != 4 || sum.SentSessions != 0 {
			t.Fatalf("skipped = %v sent = %d, want all 4 sessions org_access_lost", sum.SkippedSessions, sum.SentSessions)
		}
	})
}

type checkerFunc func(team routing.Team) error

func (f checkerFunc) Check(_ context.Context, team routing.Team, _ []string) (HeldSet, error) {
	if err := f(team); err != nil {
		return HeldSet{}, err
	}
	return NewHeldSet(), nil
}

// A planned session that turns out to be in progress when its turn comes ends in a
// session_skipped event with its place in the plan.
func TestPlannedSessionSkippedDuringUploadIsReported(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	st := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	// Discovery does not mark it active; the uploader's own check, just before reading,
	// finds it changed within the window.
	src := &stubHistorySource{
		source: "claude",
		files:  []agents.HistoryFile{{SourceSystem: "claude", SessionID: "s-live", ModTime: now.Add(-10 * time.Minute)}},
		segments: map[string][]agents.HistorySegment{
			"s-live": {makeNativeSegment("claude", "s-live", repoA, "p", now.Add(-10*time.Minute))},
		},
	}
	var events []ProgressEvent
	sum, err := Upload(context.Background(), Options{
		StateDir: t.TempDir(), Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: st, Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
		Transport: &Transport{Version: "test", HTTP: http.DefaultClient, BytesPerSec: 1 << 30},
		Now:       func() time.Time { return now },
		Progress:  func(ev ProgressEvent) { events = append(events, ev) },
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := checkPlanAccounting(t, events)
	last := events[len(events)-1]
	if plan.Sessions != 1 || last.Kind != ProgressSessionSkipped || last.SkipReason != SkipActive || last.Overall != 1 || sum.SkippedSessions[SkipActive] != 1 {
		t.Fatalf("plan %d sessions, last event %+v, skipped %v; want the one planned session skipped as active", plan.Sessions, last, sum.SkippedSessions)
	}
}

// A zero window counts nothing as changed within it, a modification time in the future
// (clock skew, a restored file) included.
func TestChangedWithinZeroWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	if changedWithin(now, now.Add(time.Hour), 0) || changedWithin(now, now, 0) {
		t.Error("a zero window counted a file as changed within it")
	}
	if !changedWithin(now, now.Add(-time.Minute), time.Hour) || changedWithin(now, now.Add(-2*time.Hour), time.Hour) {
		t.Error("a one-hour window misjudged a file")
	}
}

// A server-key profile without the team's key stops the team whole while planning, in one
// send_failed issue, without asking its server what it holds.
func TestPlanStopsATeamWithoutItsServerKey(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	st := singleTeamRoutingState(now, "no_person", "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	stateDir := t.TempDir()
	rs := NewRouteState("no_person", "default", "enr-team-a", "team-a")
	rs.MarkInProgress("claude", "s-resume", now.Add(-time.Hour), 1, 100, time.Time{})
	if err := SaveState(stateDir, rs); err != nil {
		t.Fatal(err)
	}
	src := &stubHistorySource{
		source: "claude",
		files: []agents.HistoryFile{
			{SourceSystem: "claude", SessionID: "s-resume", ModTime: now.Add(-5 * time.Hour)},
			{SourceSystem: "claude", SessionID: "s-new", ModTime: now.Add(-4 * time.Hour)},
		},
		segments: map[string][]agents.HistorySegment{
			"s-resume": {makeNativeSegment("claude", "s-resume", repoA, "r", now.Add(-5*time.Hour))},
			"s-new":    {makeNativeSegment("claude", "s-new", repoA, "n", now.Add(-4*time.Hour))},
		},
	}
	var asked []string
	var events []ProgressEvent
	sum, err := Upload(context.Background(), Options{
		StateDir: stateDir, Profile: "default", Account: "no_person", AuthURL: "https://auth.example.com", Version: "test",
		ServerKey: true, ServerKeyTeam: "team-a", ServerKeyAuthURL: "https://auth.example.com",
		ServerKeyFor: func(string) (string, error) { return "", nil },
		State:        st, Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil),
		Holdings:  checkerFunc(func(team routing.Team) error { asked = append(asked, team.TeamID); return nil }),
		Transport: &Transport{Version: "test", HTTP: http.DefaultClient, BytesPerSec: 1 << 30},
		Now:       func() time.Time { return now },
		Progress:  func(ev ProgressEvent) { events = append(events, ev) },
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := checkPlanAccounting(t, events)
	if len(asked) != 0 || plan.Sessions != 0 {
		t.Errorf("holdings asked of %v and %d sessions planned, want neither", asked, plan.Sessions)
	}
	if len(sum.TeamIssues) != 1 || sum.TeamIssues[0].Skipped != 2 || sum.SkippedSessions[SkipSendFailed] != 2 ||
		sum.TeamIssues[0].Reason != "no server key available for this team" {
		t.Fatalf("issues = %+v skipped = %v, want one send_failed issue of both sessions", sum.TeamIssues, sum.SkippedSessions)
	}
}

type cancelDiscoverSource struct {
	*stubHistorySource
}

func (s *cancelDiscoverSource) DiscoverHistory(_ context.Context, _ agents.HistoryDiscoverOptions) ([]agents.HistoryFile, error) {
	return nil, context.Canceled
}

func TestUploadInterruptDuringDiscovery(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	st := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	src := &cancelDiscoverSource{stubHistorySource: &stubHistorySource{source: "claude"}}
	sum, err := Upload(context.Background(), Options{
		StateDir: t.TempDir(), Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: st, Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
		Transport: &Transport{Version: "test", HTTP: http.DefaultClient, BytesPerSec: 1 << 30},
		Now:       func() time.Time { return now },
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !sum.Interrupted {
		t.Errorf("summary.Interrupted = false, want true when discovery is interrupted")
	}
}

func TestUploadSplitTranscriptDeduplicatesSourceBytes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	repoB := initTestGitRepo(t, "https://github.com/org-b/repo-b.git")
	st := routing.State{
		Account: "acc1",
		Teams: []routing.Team{
			{
				EnrollmentID: "enr-a", TeamID: "team-a", OrganizationID: "org-a", AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-a/repo-a"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-a", TeamID: "team-a", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
			{
				EnrollmentID: "enr-b", TeamID: "team-b", OrganizationID: "org-b", AuthURL: "https://auth.example.com",
				Policy: config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/org-b/repo-b"}, IncludePrompts: true, IncludeToolContent: true, OrganizationID: "org-b", TeamID: "team-b", AuthURL: "https://auth.example.com", FetchedAt: now},
			},
		},
	}
	transcriptPath := filepath.Join(t.TempDir(), "split.jsonl")
	payload := make([]byte, 4321)
	if err := os.WriteFile(transcriptPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	oldMod := now.Add(-4 * time.Hour)
	if err := os.Chtimes(transcriptPath, oldMod, oldMod); err != nil {
		t.Fatal(err)
	}
	src := &stubHistorySource{
		source: "claude",
		files: []agents.HistoryFile{
			{SourceSystem: "claude", SessionID: "s-split", Path: transcriptPath, ModTime: now.Add(-4 * time.Hour)},
		},
		segments: map[string][]agents.HistorySegment{
			"s-split": {
				makeNativeSegment("claude", "s-split", repoA, "part a", now.Add(-4*time.Hour)),
				makeNativeSegment("claude", "s-split", repoB, "part b", now.Add(-4*time.Hour+time.Minute)),
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var events []ProgressEvent
	sum, err := Upload(context.Background(), Options{
		StateDir: t.TempDir(), Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: st, Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
		Transport:   &Transport{Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1", tokens: []string{"t-split"}}, BytesPerSec: 1 << 30},
		EndpointFor: func(string) string { return srv.URL },
		Now:         func() time.Time { return now },
		Progress:    func(ev ProgressEvent) { events = append(events, ev) },
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := checkPlanAccounting(t, events)
	if plan.Sessions != 2 || plan.SourceBytes != int64(len(payload)) {
		t.Fatalf("plan = (sessions=%d, source_bytes=%d), want (2, %d)", plan.Sessions, plan.SourceBytes, len(payload))
	}
	var doneBytes int64
	doneCount := 0
	for _, ev := range events {
		if ev.Kind == ProgressSessionDone {
			doneCount++
			doneBytes += ev.Bytes
		}
	}
	if doneCount != 2 || sum.SentSessions != 2 || sum.SentBytes != doneBytes {
		t.Fatalf("doneCount=%d sum.SentSessions=%d sum.SentBytes=%d doneBytes=%d", doneCount, sum.SentSessions, sum.SentBytes, doneBytes)
	}
}

func TestUploadTeamBlockedAtDiscoveryEmitsRouteBlocked(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	st := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	st.Teams[0].Refusals = 1
	src := &stubHistorySource{
		source: "claude",
		files: []agents.HistoryFile{
			{SourceSystem: "claude", SessionID: "s1", ModTime: now.Add(-5 * time.Hour)},
			{SourceSystem: "claude", SessionID: "s2", ModTime: now.Add(-4 * time.Hour)},
		},
		segments: map[string][]agents.HistorySegment{
			"s1": {makeNativeSegment("claude", "s1", repoA, "one", now.Add(-5*time.Hour))},
			"s2": {makeNativeSegment("claude", "s2", repoA, "two", now.Add(-4*time.Hour))},
		},
	}
	var events []ProgressEvent
	sum, err := Upload(context.Background(), Options{
		StateDir: t.TempDir(), Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
		State: st, Sources: []agents.HistorySource{src}, Filter: relay.NewContentFilter(nil), Holdings: stubHoldings{},
		Transport: &Transport{Version: "test", HTTP: http.DefaultClient, BytesPerSec: 1 << 30},
		Now:       func() time.Time { return now },
		Progress:  func(ev ProgressEvent) { events = append(events, ev) },
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := checkPlanAccounting(t, events)
	if plan.Sessions != 0 {
		t.Fatalf("plan.Sessions = %d, want 0", plan.Sessions)
	}
	var blocked []ProgressEvent
	discoverIdx, blockedIdx, planIdx := -1, -1, -1
	for i, ev := range events {
		switch ev.Kind {
		case ProgressDiscover:
			discoverIdx = i
		case ProgressRouteBlocked:
			blockedIdx = i
			blocked = append(blocked, ev)
		case ProgressPlan:
			planIdx = i
		}
	}
	if len(blocked) != 1 || blocked[0].TeamID != "team-a" || blocked[0].EnrollmentID != "enr-team-a" ||
		blocked[0].Skipped != 2 || blocked[0].SkipReason != SkipTeamBlockedPrefix+string(routing.Suspended) {
		t.Fatalf("blocked events = %+v, want one team-a event with 2 skipped", blocked)
	}
	if discoverIdx >= blockedIdx || blockedIdx >= planIdx {
		t.Fatalf("order discover=%d blocked=%d plan=%d, want discover < blocked < plan", discoverIdx, blockedIdx, planIdx)
	}
	if len(sum.TeamIssues) != 0 {
		t.Errorf("sum.TeamIssues = %+v, want empty to preserve -o json compatibility", sum.TeamIssues)
	}
}

func TestUploadSignInLostPaths(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	repoA := initTestGitRepo(t, "https://github.com/org-a/repo-a.git")
	st := singleTeamRoutingState(now, "acc1", "org-a", "team-a", "github.com/org-a/repo-a", true, true)
	makeSrc := func() *stubHistorySource {
		return &stubHistorySource{
			source: "claude",
			files:  []agents.HistoryFile{{SourceSystem: "claude", SessionID: "s1", ModTime: now.Add(-4 * time.Hour)}},
			segments: map[string][]agents.HistorySegment{
				"s1": {makeNativeSegment("claude", "s1", repoA, "p", now.Add(-4*time.Hour))},
			},
		}
	}

	t.Run("holdings returns ErrNotLoggedIn", func(t *testing.T) {
		t.Parallel()
		_, err := Upload(context.Background(), Options{
			StateDir: t.TempDir(), Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
			State: st, Sources: []agents.HistorySource{makeSrc()}, Filter: relay.NewContentFilter(nil),
			Holdings:  checkerFunc(func(routing.Team) error { return auth.ErrNotLoggedIn }),
			Transport: &Transport{Version: "test", HTTP: http.DefaultClient, BytesPerSec: 1 << 30},
			Now:       func() time.Time { return now },
		})
		if _, ok := errors.AsType[*SignInLostError](err); !ok {
			t.Fatalf("err = %v, want *SignInLostError", err)
		}
	})

	t.Run("recheck returns ErrNotLoggedIn", func(t *testing.T) {
		t.Parallel()
		calls := 0
		_, err := Upload(context.Background(), Options{
			StateDir: t.TempDir(), Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
			State: st, Sources: []agents.HistorySource{makeSrc()}, Filter: relay.NewContentFilter(nil),
			Holdings: stubHoldings{},
			ReloadState: func() (routing.State, error) {
				calls++
				if calls == 1 {
					return st, nil
				}
				return routing.State{}, auth.ErrNotLoggedIn
			},
			Transport: &Transport{Version: "test", HTTP: http.DefaultClient, BytesPerSec: 1 << 30},
			Now:       func() time.Time { return now },
		})
		if _, ok := errors.AsType[*SignInLostError](err); !ok {
			t.Fatalf("err = %v, want *SignInLostError", err)
		}
	})

	t.Run("send has missing token", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		_, err := Upload(context.Background(), Options{
			StateDir: t.TempDir(), Profile: "default", Account: "acc1", AuthURL: "https://auth.example.com", Version: "test",
			State: st, Sources: []agents.HistorySource{makeSrc()}, Filter: relay.NewContentFilter(nil),
			Holdings:    stubHoldings{},
			Transport:   &Transport{Version: "test", HTTP: srv.Client(), SignIn: &fakeSignIn{account: "acc1"}, BytesPerSec: 1 << 30},
			EndpointFor: func(string) string { return srv.URL },
			Now:         func() time.Time { return now },
		})
		if _, ok := errors.AsType[*SignInLostError](err); !ok {
			t.Fatalf("err = %v, want *SignInLostError", err)
		}
	})
}
