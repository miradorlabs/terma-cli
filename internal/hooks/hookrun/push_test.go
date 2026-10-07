package hookrun

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

const zero = "0000000000000000000000000000000000000000"

// A quiet push of stamped commits names every commit the remote lacks, the sessions in
// them and where it went; what the remote already has, a deletion, and a push of nothing
// stamped name nothing.
func TestPrePushRecordsTheStampedCommitsAPushSends(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := hookruntest.InitRepo(t)
	remote := t.TempDir()
	if _, err := gitx.Git(ctx, remote, "init", "-q", "--bare"); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		out, err := gitx.Git(ctx, root, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	git("remote", "add", "up", remote)
	commit := func(msg string) string {
		t.Helper()
		git("commit", "-q", "--allow-empty", "-m", msg)
		return git("rev-parse", "HEAD")
	}
	base := commit("human base")
	git("push", "-q", "up", "HEAD:refs/heads/main") // the remote has base, and up/main tracks it
	human := commit("human change")
	a := commit("agent a\n\nAgent-Session-Id: sess-a\nAgent-Tool: claude-code")
	b := commit("agent b\n\nAgent-Session-Id: sess-b\nAgent-Tool: codex\nAgent-Session-Id: sess-a\nAgent-Tool: claude-code")

	sp, _ := spool.Open(t.TempDir())
	push := func(stdin string, args ...string) []spool.Event {
		t.Helper()
		env := Env{StateDir: t.TempDir(), Now: time.Now(), Cwd: root, Policy: hookruntest.Admitting(root), Args: args,
			Stdin: strings.NewReader(stdin), Spool: sp, Version: "test", Team: "proj"}
		if err := PrePush(ctx, env); err != nil {
			t.Fatal(err)
		}
		return hookruntest.Named(hookruntest.Spooled(t, sp), semconv.TermaPushEvent)
	}

	line := fmt.Sprintf("refs/heads/main %s refs/heads/main %s\n", b, base)
	events := push(line+"refs/heads/gone "+zero+" refs/heads/gone "+base+"\n", "up", remote)
	if len(events) != 1 {
		t.Fatalf("want one terma.push, got %+v", events)
	}
	ev := events[0]
	tree, _ := filepath.EvalSymlinks(root)
	if ev.SessionID != "sess-b" || ev.Attrs[semconv.GenAIMainAgentNameKey] != "codex" ||
		!reflect.DeepEqual(ev.Attrs[semconv.TermaPushSessionIDsKey], []any{"sess-b", "sess-a"}) {
		t.Errorf("sessions: %s %v", ev.SessionID, ev.Attrs)
	}
	// Newest first; base is on the remote already, and the unstamped commit is still sent.
	if !reflect.DeepEqual(ev.Attrs[semconv.TermaPushCommitShasKey], []any{b, a, human}) ||
		hookruntest.Num(ev.Attrs[semconv.TermaPushCommitCountKey]) != 3 || ev.Attrs[semconv.TermaPushCommitShasTruncatedKey] != false {
		t.Errorf("commits = %v (want %s %s %s)", ev.Attrs, b, a, human)
	}
	refs := ev.Attrs[semconv.TermaPushRefsKey].([]any)
	if len(refs) != 1 || refs[0].(map[string]any)["local_sha"] != b || refs[0].(map[string]any)["remote_ref"] != "refs/heads/main" {
		t.Errorf("refs = %v: the deletion is left out", refs)
	}
	if ev.Attrs[semconv.TermaPushRemoteNameKey] != "up" || ev.Attrs[semconv.TermaRepositoryRootKey] != tree {
		t.Errorf("remote, root = %v, %v", ev.Attrs[semconv.TermaPushRemoteNameKey], ev.Attrs[semconv.TermaRepositoryRootKey])
	}

	// Pushed to a URL, to the remote that lacks everything: the new branch sends all four,
	// and the event names the repository pushed to, not origin.
	url := "https://github.com/acme/fork.git"
	events = push(fmt.Sprintf("refs/heads/main %s refs/heads/new %s\n", b, zero), url, url)
	if len(events) != 1 || len(events[0].Attrs[semconv.TermaPushCommitShasKey].([]any)) != 4 ||
		events[0].Attrs[semconv.TermaPushRemoteNameKey] != nil || events[0].Attrs[semconv.VCSRepositoryNameKey] != "fork" ||
		events[0].Attrs[semconv.VCSRepositoryURLFullKey] != "https://github.com/acme/fork" {
		t.Errorf("a push to a URL = %+v", events)
	}

	// Nothing stamped in what is sent, nothing to push, or a deletion alone: no event.
	for _, stdin := range []string{
		fmt.Sprintf("refs/heads/main %s refs/heads/main %s\n", human, base),
		"",
		"refs/heads/gone " + zero + " refs/heads/gone " + base + "\n",
	} {
		if events := push(stdin, "up", remote); len(events) != 0 {
			t.Errorf("stdin %q spooled %+v", stdin, events)
		}
	}
}
