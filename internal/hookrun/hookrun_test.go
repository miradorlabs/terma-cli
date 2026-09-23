package hookrun

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/trailer"
)

func initRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	ctx := context.Background()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "dev@example.com"},
		{"config", "user.name", "Dev"},
		{"config", "commit.gpgsign", "false"},
	} {
		if _, err := gitx.Git(ctx, dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	resolved, _ := filepath.EvalSymlinks(dir)
	return resolved
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeSessionStampsOnlyItsOwnFiles(t *testing.T) {
	root := initRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	now := time.Now()
	env := func(stdin string, args ...string) Env {
		return Env{Now: now, Cwd: root, Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}

	if err := SessionStart(ctx, env(`{"session_id":"sess-claude-1","cwd":"`+root+`","hook_event_name":"SessionStart","source":"startup","model":"claude-opus-5"}`)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "src/agent.go", "package src\n")
	writeFile(t, root, "notes/human.md", "mine\n")
	if err := PostToolUse(ctx, env(`{"session_id":"sess-claude-1","cwd":"`+root+`","tool_name":"Write","tool_input":{"file_path":"`+filepath.Join(root, "src", "agent.go")+`"}}`)); err != nil {
		t.Fatal(err)
	}

	// Commit only the human file: no trailer.
	if _, err := gitx.Git(ctx, root, "add", "notes/human.md"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	_ = os.WriteFile(msgPath, []byte("human note\n"), 0o644)
	if err := PrepareCommitMsg(ctx, env("", msgPath, "message")); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(msgPath); strings.Contains(string(data), "Agent-Session-Id") {
		t.Fatalf("human-only commit must not be stamped:\n%s", data)
	}
	if _, err := gitx.Git(ctx, root, "commit", "-q", "-F", msgPath); err != nil {
		t.Fatal(err)
	}

	// Now the agent's file: stamped with the session that touched it.
	if _, err := gitx.Git(ctx, root, "add", "src/agent.go"); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(msgPath, []byte("Add agent code\n\n# comment\n"), 0o644)
	start := time.Now()
	if err := PrepareCommitMsg(ctx, env("", msgPath, "")); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Logf("warning: prepare-commit-msg took %s", elapsed)
	}
	data, _ := os.ReadFile(msgPath)
	got := trailer.Parse(string(data), "#")
	if len(got) != 1 || got[0].SessionID != "sess-claude-1" || got[0].Tool != "claude-code" {
		t.Fatalf("unexpected trailers %+v in:\n%s", got, data)
	}
	if _, err := gitx.Git(ctx, root, "commit", "-q", "-F", msgPath); err != nil {
		t.Fatal(err)
	}
	if err := PostCommit(ctx, env("")); err != nil {
		t.Fatal(err)
	}

	// The committed file is consumed: a later commit of unrelated work is clean,
	// even though the session is still active.
	writeFile(t, root, "notes/again.md", "more\n")
	_, _ = gitx.Git(ctx, root, "add", "notes/again.md")
	_ = os.WriteFile(msgPath, []byte("more notes\n"), 0o644)
	_ = PrepareCommitMsg(ctx, env("", msgPath, ""))
	if data, _ := os.ReadFile(msgPath); strings.Contains(string(data), "Agent-Session-Id") {
		t.Fatalf("work already committed must not re-stamp a later human commit, even with the session still active:\n%s", data)
	}

	// Spool: start, account snapshot, files touched, stamped, commit.
	n, _, _ := sp.Pending()
	if n != 5 {
		t.Fatalf("expected 5 spooled events, got %d", n)
	}
	var names []string
	res := sp.Flush(ctx, spool.SenderFunc(func(_ context.Context, events []spool.Event) ([]spool.Event, error) {
		for _, e := range events {
			names = append(names, e.Name)
		}
		return nil, nil
	}), spool.FlushOptions{})
	if res.Err != nil || strings.Join(names, " ") != "terma.session.start terma.session.account terma.files.touched terma.commit.stamped terma.commit" {
		t.Fatalf("unexpected events %v (%v)", names, res.Err)
	}
}

func TestActiveSessionFallbackAndMergeSkip(t *testing.T) {
	root := initRepo(t)
	ctx := context.Background()
	now := time.Now()
	env := func(stdin string, args ...string) Env {
		return Env{Now: now, Cwd: root, Args: args, Stdin: strings.NewReader(stdin), Version: "test"}
	}
	// Codex announces a thread but reports no files: fallback attribution.
	if err := CodexNotify(ctx, env("", `{"type":"agent-turn-complete","thread-id":"thread-9","cwd":"`+root+`","model":"gpt-5.4"}`)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "a.txt", "a\n")
	_, _ = gitx.Git(ctx, root, "add", "a.txt")
	msgPath := filepath.Join(t.TempDir(), "MSG")
	_ = os.WriteFile(msgPath, []byte("work\n"), 0o644)
	_ = PrepareCommitMsg(ctx, env("", msgPath, ""))
	data, _ := os.ReadFile(msgPath)
	if got := trailer.Parse(string(data), "#"); len(got) != 1 || got[0].SessionID != "thread-9" || got[0].Tool != "codex" {
		t.Fatalf("expected the codex thread as fallback, got %+v", got)
	}

	// Merge commits are never stamped.
	_ = os.WriteFile(msgPath, []byte("Merge branch 'x'\n"), 0o644)
	_ = PrepareCommitMsg(ctx, env("", msgPath, "merge"))
	if data, _ := os.ReadFile(msgPath); strings.Contains(string(data), "Agent-Session-Id") {
		t.Fatal("merge message was stamped")
	}

	// Expired fallback: nothing.
	late := env("", msgPath, "")
	late.Now = now.Add(ActiveTTL + time.Minute)
	_ = os.WriteFile(msgPath, []byte("later\n"), 0o644)
	_ = PrepareCommitMsg(ctx, late)
	if data, _ := os.ReadFile(msgPath); strings.Contains(string(data), "Agent-Session-Id") {
		t.Fatal("stale session must not claim a commit")
	}
}

func TestHandlersNeverFailOutsideARepo(t *testing.T) {
	ctx := context.Background()
	env := Env{Cwd: t.TempDir(), Stdin: strings.NewReader(`{"session_id":"s"}`), Args: []string{"/nonexistent"}}
	for name, fn := range map[string]func(context.Context, Env) error{
		"start": SessionStart, "end": SessionEnd, "tool": PostToolUse,
		"prepare": PrepareCommitMsg, "post": PostCommit, "codex": CodexNotify,
	} {
		if err := fn(ctx, env); err != nil {
			t.Errorf("%s returned %v outside a repo", name, err)
		}
	}
	bad := Env{Cwd: t.TempDir(), Stdin: strings.NewReader("not json")}
	if err := SessionStart(ctx, bad); err != nil {
		t.Errorf("garbage input must be ignored, got %v", err)
	}
}

// commitEvent flushes the spool and returns the attributes of the single
// terma.commit event in it.
func commitEvent(t *testing.T, sp *spool.Spool) map[string]any {
	t.Helper()
	var commits []spool.Event
	res := sp.Flush(context.Background(), spool.SenderFunc(func(_ context.Context, events []spool.Event) ([]spool.Event, error) {
		for _, e := range events {
			if e.Name == EventCommit {
				commits = append(commits, e)
			}
		}
		return nil, nil
	}), spool.FlushOptions{})
	if res.Err != nil {
		t.Fatalf("flush: %v", res.Err)
	}
	if len(commits) != 1 {
		t.Fatalf("expected exactly one %s event, got %d", EventCommit, len(commits))
	}
	return commits[0].Attrs
}

// fileStats decodes the file_stats attribute into the shape it has on the wire —
// generic maps, so the test sees the actual JSON keys rather than the Go struct.
func fileStats(t *testing.T, attrs map[string]any) []map[string]any {
	t.Helper()
	raw, ok := attrs["file_stats"].(string)
	if !ok {
		t.Fatalf("file_stats is %T, want a JSON string: %v", attrs["file_stats"], attrs)
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("file_stats is not valid JSON (%v): %s", err, raw)
	}
	return out
}

func TestPostCommitReportsPerFileLineStats(t *testing.T) {
	root := initRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	now := time.Now()
	env := func(at time.Time, stdin string, args ...string) Env {
		return Env{Now: at, Cwd: root, Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	touch := func(at time.Time, sessionID, rel string) {
		t.Helper()
		in := `{"session_id":"` + sessionID + `","cwd":"` + root + `","tool_name":"Edit","tool_input":{"file_path":"` + filepath.Join(root, rel) + `"}}`
		if err := PostToolUse(ctx, env(at, in)); err != nil {
			t.Fatal(err)
		}
	}

	for _, id := range []string{"sess-a", "sess-b"} {
		if err := SessionStart(ctx, env(now, `{"session_id":"`+id+`","cwd":"`+root+`","hook_event_name":"SessionStart","source":"startup"}`)); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, root, "src/a.go", "package a\nfunc A() {}\n")
	writeFile(t, root, "src/b.go", "package b\n")
	writeFile(t, root, "assets/logo.bin", "\x00\x01\x02logo\x00")
	writeFile(t, root, "src/shared.go", "package shared\n")
	touch(now, "sess-a", "src/a.go")
	touch(now, "sess-a", "src/shared.go")
	touch(now.Add(time.Minute), "sess-b", "src/b.go")
	touch(now.Add(time.Minute), "sess-b", "assets/logo.bin")
	// Both sessions touched shared.go; the later touch decides who owns it.
	touch(now.Add(2*time.Minute), "sess-b", "src/shared.go")

	if _, err := gitx.Git(ctx, root, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "MSG")
	_ = os.WriteFile(msgPath, []byte("feat: both sessions\n"), 0o644)
	if err := PrepareCommitMsg(ctx, env(now, "", msgPath, "")); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Git(ctx, root, "commit", "-q", "-F", msgPath); err != nil {
		t.Fatal(err)
	}
	if err := PostCommit(ctx, env(now, "")); err != nil {
		t.Fatal(err)
	}

	attrs := commitEvent(t, sp)
	for _, key := range []string{"sha", "sessions", "session_count", "tool", "file_count", "author_email"} {
		if _, ok := attrs[key]; !ok {
			t.Fatalf("existing attribute %q was dropped: %v", key, attrs)
		}
	}
	if got := attrs["file_count"]; got != float64(4) {
		t.Fatalf("file_count = %v, want 4", got)
	}
	// 2 + 1 + 1 lines of Go; the binary file contributes no counts at all.
	if got, want := attrs["lines_added"], float64(4); got != want {
		t.Fatalf("lines_added = %v, want %v", got, want)
	}
	if got, want := attrs["lines_deleted"], float64(0); got != want {
		t.Fatalf("lines_deleted = %v, want %v", got, want)
	}
	if attrs["file_stats_truncated"] != false || attrs["file_stats_reported"] != float64(4) {
		t.Fatalf("truncation attrs = %v / %v", attrs["file_stats_truncated"], attrs["file_stats_reported"])
	}

	stats := fileStats(t, attrs)
	byPath := map[string]map[string]any{}
	for _, s := range stats {
		byPath[s["path"].(string)] = s
	}
	if len(byPath) != 4 {
		t.Fatalf("file_stats = %v", stats)
	}
	if got := byPath["src/a.go"]; got["added"] != float64(2) || got["deleted"] != float64(0) || got["session_id"] != "sess-a" {
		t.Fatalf("src/a.go = %v", got)
	}
	if got := byPath["src/b.go"]; got["added"] != float64(1) || got["session_id"] != "sess-b" {
		t.Fatalf("src/b.go = %v", got)
	}
	if got := byPath["src/shared.go"]; got["session_id"] != "sess-b" {
		t.Fatalf("the later touch should own shared.go: %v", got)
	}
	// A binary file has no line counts: it says so instead of reporting zeros.
	bin := byPath["assets/logo.bin"]
	if bin["binary"] != true {
		t.Fatalf("binary file not flagged: %v", bin)
	}
	if _, ok := bin["added"]; ok {
		t.Fatalf("a binary file must not carry a line count: %v", bin)
	}
	if _, ok := bin["deleted"]; ok {
		t.Fatalf("a binary file must not carry a line count: %v", bin)
	}
}

func TestPostCommitBoundsFileStats(t *testing.T) {
	root := initRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	now := time.Now()
	env := func(stdin string, args ...string) Env {
		return Env{Now: now, Cwd: root, Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	if err := SessionStart(ctx, env(`{"session_id":"sess-wide","cwd":"`+root+`","hook_event_name":"SessionStart"}`)); err != nil {
		t.Fatal(err)
	}
	total := MaxCommitFileStats + 5
	for i := range total {
		rel := fmt.Sprintf("src/f%03d.go", i)
		writeFile(t, root, rel, "package p\n")
		if err := PostToolUse(ctx, env(`{"session_id":"sess-wide","cwd":"`+root+`","tool_name":"Write","tool_input":{"file_path":"`+filepath.Join(root, rel)+`"}}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gitx.Git(ctx, root, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "MSG")
	_ = os.WriteFile(msgPath, []byte("chore: sweep\n"), 0o644)
	if err := PrepareCommitMsg(ctx, env("", msgPath, "")); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Git(ctx, root, "commit", "-q", "-F", msgPath); err != nil {
		t.Fatal(err)
	}
	if err := PostCommit(ctx, env("")); err != nil {
		t.Fatal(err)
	}

	attrs := commitEvent(t, sp)
	if got := attrs["file_count"]; got != float64(total) {
		t.Fatalf("file_count = %v, want the true total %d", got, total)
	}
	stats := fileStats(t, attrs)
	if len(stats) != MaxCommitFileStats {
		t.Fatalf("file_stats holds %d entries, want the cap of %d", len(stats), MaxCommitFileStats)
	}
	if attrs["file_stats_truncated"] != true || attrs["file_stats_reported"] != float64(MaxCommitFileStats) {
		t.Fatalf("truncation must be visible: %v / %v", attrs["file_stats_truncated"], attrs["file_stats_reported"])
	}
	// One session: the file entries carry no session id, `sessions` says it all.
	if _, ok := stats[0]["session_id"]; ok {
		t.Fatalf("single-session commit should not repeat the session per file: %v", stats[0])
	}
}

// spooled flushes the spool and returns every event in it, in order.
func spooled(t *testing.T, sp *spool.Spool) []spool.Event {
	t.Helper()
	var out []spool.Event
	res := sp.Flush(context.Background(), spool.SenderFunc(func(_ context.Context, events []spool.Event) ([]spool.Event, error) {
		out = append(out, events...)
		return nil, nil
	}), spool.FlushOptions{})
	if res.Err != nil {
		t.Fatalf("flush: %v", res.Err)
	}
	return out
}

// keysOf is the sorted attribute names of an event: the shape a consumer sees.
func keysOf(attrs map[string]any) []string { return slices.Sorted(maps.Keys(attrs)) }

// commitIdentity is what both commit events carry — enough to identify and size a
// commit, nothing about its contents. The unattributed event is exactly this.
var commitIdentity = []string{"author_email", "branch", "file_count", "lines_added", "lines_deleted", AttrProjectID, "repo_url", "sha"}

func TestPostCommitOnAnUnstampedCommitEmitsOnlyACount(t *testing.T) {
	root := initRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	env := Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(""), Spool: sp, Version: "test"}
	// A remote carrying a credential and a project binding, so the event has every
	// field it is allowed to carry and the test can see what happened to each.
	if _, err := gitx.Git(ctx, root, "remote", "add", "origin", "https://dev:ghp_secret@github.com/o/r.git"); err != nil {
		t.Fatal(err)
	}
	if err := project.Save(root, &project.File{Project: project.Project{ID: "proj_test"}}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "notes/human.md", "mine\nall mine\n")
	if _, err := gitx.Git(ctx, root, "add", "notes/human.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Git(ctx, root, "commit", "-q", "-m", "no trailer here"); err != nil {
		t.Fatal(err)
	}
	sha, _ := gitx.Git(ctx, root, "rev-parse", "HEAD")
	if err := PostCommit(ctx, env); err != nil {
		t.Fatal(err)
	}

	events := spooled(t, sp)
	if len(events) != 1 || events[0].Name != EventCommitUnattributed {
		t.Fatalf("an unstamped commit must spool exactly one %s, got %+v", EventCommitUnattributed, events)
	}
	ev := events[0]
	if ev.SessionID != "" {
		t.Fatalf("no session was part of this commit, got session %q", ev.SessionID)
	}
	if ev.Repo != filepath.Base(root) {
		t.Fatalf("repo = %q, want %q", ev.Repo, filepath.Base(root))
	}
	if got := keysOf(ev.Attrs); !slices.Equal(got, commitIdentity) {
		t.Fatalf("the unattributed event must carry exactly the commit's identity and size\n got %v\nwant %v", got, commitIdentity)
	}
	want := map[string]any{
		"sha": sha, "author_email": "dev@example.com", "branch": "main", "repo_url": "https://github.com/o/r",
		"file_count": 1, "lines_added": 2, "lines_deleted": 0, AttrProjectID: "proj_test",
	}
	for k, v := range want {
		if fmt.Sprint(ev.Attrs[k]) != fmt.Sprint(v) {
			t.Errorf("%s = %v, want %v", k, ev.Attrs[k], v)
		}
	}
	// The boundary: a count, never a manifest. No value may name the file, and the
	// credential in the remote must not survive.
	blob, _ := json.Marshal(ev.Attrs)
	for _, leak := range []string{"human.md", "notes", "ghp_secret", "file_stats"} {
		if strings.Contains(string(blob), leak) {
			t.Fatalf("unattributed event leaks %q: %s", leak, blob)
		}
	}
}

// The coverage query is one filter on the event name, grouped by name: the two
// commit events must be told apart by it, share the identity attributes, and the
// stamped one must be the pre-existing terma.commit, untouched.
func TestCommitEventsAreOneFilterApart(t *testing.T) {
	root := initRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	now := time.Now()
	env := func(stdin string, args ...string) Env {
		return Env{Now: now, Cwd: root, Args: args, Stdin: strings.NewReader(stdin), Spool: sp, Version: "test"}
	}
	if _, err := gitx.Git(ctx, root, "remote", "add", "origin", "git@github.com:o/r.git"); err != nil {
		t.Fatal(err)
	}
	if err := project.Save(root, &project.File{Project: project.Project{ID: "proj_test"}}); err != nil {
		t.Fatal(err)
	}
	msgPath := filepath.Join(t.TempDir(), "MSG")
	commit := func(rel, msg string) string {
		t.Helper()
		if _, err := gitx.Git(ctx, root, "add", rel); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(msgPath, []byte(msg+"\n"), 0o644)
		if err := PrepareCommitMsg(ctx, env("", msgPath, "message")); err != nil {
			t.Fatal(err)
		}
		if _, err := gitx.Git(ctx, root, "commit", "-q", "-F", msgPath); err != nil {
			t.Fatal(err)
		}
		if err := PostCommit(ctx, env("")); err != nil {
			t.Fatal(err)
		}
		sha, _ := gitx.Git(ctx, root, "rev-parse", "HEAD")
		return sha
	}

	// A human commit, then an agent commit.
	writeFile(t, root, "notes/human.md", "mine\n")
	humanSHA := commit("notes/human.md", "human note")
	if err := SessionStart(ctx, env(`{"session_id":"sess-1","cwd":"`+root+`","hook_event_name":"SessionStart"}`)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "src/agent.go", "package src\n")
	if err := PostToolUse(ctx, env(`{"session_id":"sess-1","cwd":"`+root+`","tool_name":"Write","tool_input":{"file_path":"`+filepath.Join(root, "src", "agent.go")+`"}}`)); err != nil {
		t.Fatal(err)
	}
	agentSHA := commit("src/agent.go", "Add agent code")

	// The spool also holds terma.commit.stamped — prepare-commit-msg's record of the
	// agent commit — which a prefix match would count a second time. The filter is
	// an exact set of two names.
	byName := map[string][]spool.Event{}
	var all []string
	for _, ev := range spooled(t, sp) {
		all = append(all, ev.Name)
		if ev.Name == EventCommit || ev.Name == EventCommitUnattributed {
			byName[ev.Name] = append(byName[ev.Name], ev)
		}
	}
	if !slices.Contains(all, EventCommitStamped) {
		t.Fatalf("expected prepare-commit-msg's record in the spool as well: %v", all)
	}
	if len(byName[EventCommit]) != 1 || len(byName[EventCommitUnattributed]) != 1 {
		t.Fatalf("want one stamped and one unstamped commit event, got %v", all)
	}
	stamped, unstamped := byName[EventCommit][0], byName[EventCommitUnattributed][0]
	if stamped.Attrs["sha"] != agentSHA || unstamped.Attrs["sha"] != humanSHA {
		t.Fatalf("shas: stamped=%v (want %s) unstamped=%v (want %s)", stamped.Attrs["sha"], agentSHA, unstamped.Attrs["sha"], humanSHA)
	}

	// terma.commit is unchanged: the identity attributes plus the session and the
	// per-file detail, with its session id on the event.
	wantStamped := slices.Sorted(slices.Values(append(slices.Clone(commitIdentity),
		"file_stats", "file_stats_reported", "file_stats_truncated", "session_count", "sessions", "tool")))
	if got := keysOf(stamped.Attrs); !slices.Equal(got, wantStamped) {
		t.Fatalf("terma.commit changed shape\n got %v\nwant %v", got, wantStamped)
	}
	if stamped.SessionID != "sess-1" || stamped.Attrs["sessions"] != "sess-1" || stamped.Attrs["tool"] != "claude-code" {
		t.Fatalf("terma.commit lost its session: %+v", stamped)
	}
	if got := keysOf(unstamped.Attrs); !slices.Equal(got, commitIdentity) {
		t.Fatalf("unattributed event shape\n got %v\nwant %v", got, commitIdentity)
	}
	// Both carry the same identity, so a dashboard can compare the two populations.
	for _, k := range []string{"author_email", "branch", "repo_url", AttrProjectID} {
		if stamped.Attrs[k] != unstamped.Attrs[k] {
			t.Errorf("%s differs between the two commit events: %v vs %v", k, stamped.Attrs[k], unstamped.Attrs[k])
		}
	}
}

// Merge and squash commits are never stamped, so counting them would only ever
// lower coverage; post-commit leaves them out the way prepare-commit-msg does.
func TestPostCommitSkipsMergeAndSquashCommits(t *testing.T) {
	root := initRepo(t)
	ctx := context.Background()
	sp, _ := spool.Open(t.TempDir())
	env := Env{Now: time.Now(), Cwd: root, Stdin: strings.NewReader(""), Spool: sp, Version: "test"}
	git := func(args ...string) {
		t.Helper()
		if _, err := gitx.Git(ctx, root, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	commit := func(rel, msg string) {
		t.Helper()
		writeFile(t, root, rel, msg+"\n")
		git("add", rel)
		git("commit", "-q", "-m", msg)
	}
	pending := func() int { n, _, _ := sp.Pending(); return n }

	commit("base.txt", "base")
	git("checkout", "-q", "-b", "feature")
	commit("feature.txt", "feature work")
	git("checkout", "-q", "main")
	commit("main.txt", "mainline work")

	// A merge commit: two parents.
	git("merge", "-q", "--no-ff", "--no-edit", "feature")
	if err := PostCommit(ctx, env); err != nil {
		t.Fatal(err)
	}
	if n := pending(); n != 0 {
		t.Fatalf("a merge commit must spool nothing, got %d event(s)", n)
	}

	// A squash: one parent, git's default subject.
	git("checkout", "-q", "-b", "feature2")
	commit("feature2.txt", "more feature work")
	git("checkout", "-q", "main")
	git("merge", "-q", "--squash", "feature2")
	git("commit", "-q", "--no-edit")
	if err := PostCommit(ctx, env); err != nil {
		t.Fatal(err)
	}
	if n := pending(); n != 0 {
		t.Fatalf("a squash commit must spool nothing, got %d event(s)", n)
	}

	// The skip is specific: the next ordinary commit is counted.
	commit("after.txt", "ordinary work")
	if err := PostCommit(ctx, env); err != nil {
		t.Fatal(err)
	}
	if n := pending(); n != 1 {
		t.Fatalf("an ordinary unstamped commit must spool one event, got %d", n)
	}
}
