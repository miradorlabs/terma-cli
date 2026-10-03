package claim

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func enable(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TERMA_CONFIG_DIR", dir)
	if err := os.MkdirAll(filepath.Join(dir, DirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DirName, tokenFile), []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWriteReadRefresh(t *testing.T) {
	enable(t)
	if !Enabled() {
		t.Fatal("a token means the relay is set up")
	}
	now := time.Now()
	if !Write("sess-1", Claim{ProjectID: "p1", Tool: "codex"}, now) {
		t.Fatal("first claim not written")
	}
	if Write("sess-1", Claim{ProjectID: "p1"}, now.Add(time.Minute)) {
		t.Fatal("a fresh claim for the same project is not rewritten")
	}
	if !Write("sess-1", Claim{ProjectID: "p2"}, now.Add(time.Minute)) {
		t.Fatal("a claim for another project is rewritten at once")
	}
	c, ok := Read("sess-1", now.Add(time.Minute))
	if !ok || c.ProjectID != "p2" {
		t.Fatalf("read = %+v, %v", c, ok)
	}
}

func TestReadRejectsStaleAndUnsafe(t *testing.T) {
	dir := enable(t)
	now := time.Now()
	Write("sess-1", Claim{ProjectID: "p1"}, now)
	p := filepath.Join(dir, DirName, claimsDir, "sess-1.json")
	old := now.Add(-TTL - time.Minute)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if _, ok := Read("sess-1", now); ok {
		t.Fatal("a claim past TTL is not live")
	}
	Prune(now)
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("prune keeps a stale claim")
	}
	for _, bad := range []string{"", "../x", ".hidden", "a/b"} {
		if Write(bad, Claim{ProjectID: "p1"}, now) {
			t.Fatalf("unsafe session id %q was written", bad)
		}
	}
	if Write("sess-2", Claim{}, now) {
		t.Fatal("a claim without a project is not written")
	}
}

// Each hook adds its processes; one whose processes are already named, inside Refresh, writes nothing.
func TestClaimMergesProcesses(t *testing.T) {
	enable(t)
	now := time.Now()
	Write("s", Claim{ProjectID: "p", PIDs: []int{10, 11}}, now)
	if Write("s", Claim{ProjectID: "p", PIDs: []int{11}}, now) {
		t.Fatal("known processes rewrote a fresh claim")
	}
	if !Write("s", Claim{ProjectID: "p", PIDs: []int{20, 21}}, now) {
		t.Fatal("a new run of the session did not add its processes")
	}
	c, _ := Read("s", now)
	if !c.Covers(10) || !c.Covers(21) || c.Covers(99) {
		t.Fatalf("claim %v", c.PIDs)
	}
	if !(Claim{}).Covers(99) || !(Claim{}).Covers(0) {
		t.Fatal("a claim naming no processes (a platform that cannot read them) covers any sender")
	}
	if c.Covers(0) {
		t.Fatal("a sender the relay could not identify widened a claim that names its processes")
	}
}

// Concurrent hooks of one session each add their processes: none is lost.
func TestConcurrentWritersKeepEveryProcess(t *testing.T) {
	enable(t)
	saved := lockWait
	lockWait = time.Minute // exclusion under test, not the machine's speed
	t.Cleanup(func() { lockWait = saved })
	now := time.Now()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Write("s", Claim{ProjectID: "p", PIDs: []int{1000 + i}}, now)
		}()
	}
	wg.Wait()
	c, ok := Read("s", now)
	if !ok {
		t.Fatal("no claim")
	}
	for i := range 16 {
		if !c.Covers(1000 + i) {
			t.Fatalf("writer %d's process was lost: %v", i, c.PIDs)
		}
	}
}

// A session resumed in another repository gets a second placement; process, else time, picks the project.
func TestClaimKeepsEachPlacementOfAResumedSession(t *testing.T) {
	enable(t)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	t1 := t0.Add(30 * time.Minute)
	Write("s", Claim{ProjectID: "p1", Tool: "claude-code", PIDs: []int{10}}, t0)
	if !Write("s", Claim{ProjectID: "p2", Tool: "claude-code", PIDs: []int{20}}, t1) {
		t.Fatal("a resume in another project did not write")
	}
	c, ok := Read("s", t1)
	if !ok || len(c.Placements) != 2 || c.ProjectID != "p2" {
		t.Fatalf("claim %+v", c)
	}
	for _, tc := range []struct {
		pid  int
		at   time.Time
		want string
	}{
		{10, t1.Add(time.Minute), "p1"}, // the first run's process, however late
		{20, t0, "p2"},                  // the second run's, however early its clock
	} {
		got, ok := c.At(tc.pid, tc.at)
		if !ok || got.ProjectID != tc.want {
			t.Errorf("At(%d, %v) = %q, %v; want %q", tc.pid, tc.at, got.ProjectID, ok, tc.want)
		}
	}
	if _, ok := c.At(0, t1); ok {
		t.Fatal("a sender the relay could not identify was covered by placements that name processes")
	}
	// Where processes cannot be read, the placements name none: a record's time decides.
	blind := Claim{ProjectID: "p2", Placements: []Placement{{ProjectID: "p1", Since: t0}, {ProjectID: "p2", Since: t1}}}
	for _, tc := range []struct {
		at   time.Time
		want string
	}{{t0.Add(time.Minute), "p1"}, {t1.Add(time.Minute), "p2"}, {time.Time{}, "p2"}} {
		if got, ok := blind.At(0, tc.at); !ok || got.ProjectID != tc.want {
			t.Errorf("blind At(0, %v) = %q, %v; want %q", tc.at, got.ProjectID, ok, tc.want)
		}
	}
	if _, ok := c.At(99, t1); ok {
		t.Fatal("a process neither run named is covered")
	}
	// Back in the first repository: a third placement, not a merge into the first.
	Write("s", Claim{ProjectID: "p1", PIDs: []int{30}}, t1.Add(time.Minute))
	c, _ = Read("s", t1.Add(time.Minute))
	if len(c.Placements) != 3 {
		t.Fatalf("placements %+v", c.Placements)
	}
	if got, _ := c.At(20, time.Time{}); got.ProjectID != "p2" {
		t.Fatalf("the middle run moved: %+v", got)
	}
}

// A claim without placements has its top-level fields as its one placement.
func TestClaimWithoutPlacements(t *testing.T) {
	c := Claim{ProjectID: "p", PIDs: []int{10}}
	if got, ok := c.At(10, time.Now()); !ok || got.ProjectID != "p" {
		t.Fatalf("At = %+v, %v", got, ok)
	}
	if _, ok := c.At(11, time.Now()); ok {
		t.Fatal("covered another process")
	}
}

// A hook reading the claim while another writes it costs that write nothing: Windows
// refuses to replace a file someone holds open, and the writer's process was lost.
func TestWriteSurvivesAReaderHoldingTheClaim(t *testing.T) {
	enable(t)
	now := time.Now()
	if !Write("s", Claim{ProjectID: "p", PIDs: []int{1}}, now) {
		t.Fatal("first write")
	}
	p, _ := path("s")
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(20 * time.Millisecond); f.Close() }()
	if !Write("s", Claim{ProjectID: "p", PIDs: []int{2}}, now) {
		t.Fatal("a write while a reader held the claim was lost")
	}
	if c, _ := Read("s", now); !c.Covers(1) || !c.Covers(2) {
		t.Fatalf("claim %v", c.PIDs)
	}
}

// A mark names the agent and its processes, and nothing that says whose or where.
func TestMarkNamesNoTeamNoRepository(t *testing.T) {
	enable(t)
	now := time.Now()
	if !Mark("s", "codex", []int{10, 11}, now) {
		t.Fatal("mark not written")
	}
	p, _ := path("s")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"repository"`, `"repo"`, `"worktree"`} {
		if strings.Contains(string(data), field) {
			t.Errorf("the mark holds %s: %s", field, data)
		}
	}
	c, ok := Read("s", now)
	if !ok || c.ProjectID != "" || len(c.Placements) != 1 || c.Placements[0].ProjectID != "" || c.Tool != "codex" || !c.Covers(11) {
		t.Fatalf("read = %+v, %v", c, ok)
	}
	if got, ok := c.At(10, now); !ok || got.ProjectID != "" {
		t.Fatalf("At = %+v, %v", got, ok)
	}
}

// A session's hooks mark it once; only a new process or the TTL's half writes again.
func TestMarkWritesOncePerSession(t *testing.T) {
	enable(t)
	now := time.Now()
	Mark("s", "claude-code", []int{10}, now)
	if Mark("s", "claude-code", []int{10}, now.Add(30*time.Minute)) {
		t.Fatal("a fresh mark naming these processes was rewritten")
	}
	if !Mark("s", "claude-code", []int{20}, now.Add(30*time.Minute)) {
		t.Fatal("a new process was not added")
	}
	if !Mark("s", "claude-code", []int{20}, now.Add(TTL/2+time.Minute)) {
		t.Fatal("a mark half its TTL old was not refreshed")
	}
	if c, _ := Read("s", now); len(c.Placements) != 1 || !c.Covers(10) || !c.Covers(20) {
		t.Fatalf("claim %+v", c)
	}
}

// At carries the selected placement's project, never the top level's: a mark then a
// claim, and a claim then a mark, each place a record by its process and time.
func TestAtAcrossMarkAndClaim(t *testing.T) {
	enable(t)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	t1 := t0.Add(30 * time.Minute)
	Mark("unlisted-first", "codex", []int{10}, t0)
	Write("unlisted-first", Claim{ProjectID: "p1", PIDs: []int{10}}, t1)
	Write("listed-first", Claim{ProjectID: "p1", PIDs: []int{10}}, t0)
	Mark("listed-first", "codex", []int{10}, t1)
	for _, tc := range []struct {
		session string
		pid     int
		at      time.Time
		top     string
		want    string
	}{
		{"unlisted-first", 10, t0.Add(time.Minute), "p1", ""}, // top level listed, the mark selected
		{"unlisted-first", 10, t1.Add(time.Minute), "p1", "p1"},
		{"listed-first", 10, t0.Add(time.Minute), "", "p1"}, // top level a mark, the claim selected
		{"listed-first", 10, t1.Add(time.Minute), "", ""},
	} {
		c, ok := Read(tc.session, t1)
		if !ok || c.ProjectID != tc.top {
			t.Fatalf("%s: read %+v, %v", tc.session, c, ok)
		}
		if got, ok := c.At(tc.pid, tc.at); !ok || got.ProjectID != tc.want {
			t.Errorf("%s At(%d, %v) = %q, %v; want %q", tc.session, tc.pid, tc.at, got.ProjectID, ok, tc.want)
		}
	}
	// A listed run in another process: its first records, before its placement, are its own.
	Mark("resumed", "codex", []int{10}, t0)
	Write("resumed", Claim{ProjectID: "p1", PIDs: []int{20}}, t1)
	c, _ := Read("resumed", t1)
	if got, ok := c.At(20, t0.Add(time.Minute)); !ok || got.ProjectID != "p1" {
		t.Fatalf("the resumed run's early record = %+v, %v", got, ok)
	}
}
