package relay

import (
	"slices"
	"strings"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// spoofedCheckout is a resource whose agent names a checkout of its own, which the relay's
// claim replaces.
func spoofedCheckout() []*commonpb.KeyValue {
	out := []*commonpb.KeyValue{kv("service.name", "agent")}
	for _, key := range slices.Concat(checkoutSafeKeys, checkoutContentKeys) {
		out = append(out, kv(key, "agent-said"))
	}
	return out
}

// A claimed session's records name its repository and branch, and whether its checkout is a
// linked worktree, under any policy: Claude Code's (A) in the main checkout, Codex's (B) in a
// linked worktree. The worktree's name, like the paths, leaves only with tool content. Whatever
// the agent named itself is gone.
func TestRelayStampsTheCheckout(t *testing.T) {
	t.Parallel()
	for _, codexContent := range []bool{true, false} {
		u := newUpstream(t)
		f := newFixture()
		f.claim("A", claim.Claim{ProjectID: "p1", Tool: "claude-code", Repository: config.Repository{Origin: "github.com/acme/web"},
			Root: "/work/web", Cwd: "/work/web", Remote: "https://github.com/acme/web", Branch: "main"})
		f.claim("B", claim.Claim{ProjectID: "p2", Tool: "codex", Repository: config.Repository{Origin: "gitlab.com/acme/tools/cli"},
			Root: "/work/cli-parser", Cwd: "/work/cli-parser", Worktree: "cli-parser", Remote: "https://gitlab.com/acme/tools/cli", Branch: "feature/parser"})
		policies := allPolicies(u)
		policies["p2"] = Policy{Endpoint: u.srv.URL, Key: "key-p2", IncludePrompts: codexContent, IncludeToolContent: codexContent}
		r, srv := f.relay(t, u, policies)
		logs := mixedLogs()
		logs.ResourceLogs[0].Resource = &resourcepb.Resource{Attributes: spoofedCheckout()}
		body, _ := proto.Marshal(logs)
		post(t, srv, "/v1/logs", body, "application/x-protobuf", token, false)
		waitFor(t, func() bool { return r.Stats().Snapshot().Counters["forwarded.logs"] == 3 })
		worktree, root := "", ""
		if codexContent {
			worktree, root = "cli-parser", "/work/cli-parser"
		}
		u.checkStamps(t, map[string]map[string]string{
			semconv.VCSRepositoryURLFullKey:          {"Bearer key-p1": "https://github.com/acme/web", "Bearer key-p2": "https://gitlab.com/acme/tools/cli"},
			semconv.VCSOwnerNameKey:                  {"Bearer key-p1": "acme", "Bearer key-p2": "acme/tools"},
			semconv.VCSRepositoryNameKey:             {"Bearer key-p1": "web", "Bearer key-p2": "cli"},
			semconv.VCSProviderNameKey:               {"Bearer key-p1": semconv.VCSProviderNameGithub, "Bearer key-p2": semconv.VCSProviderNameGitlab},
			semconv.VCSRefHeadNameKey:                {"Bearer key-p1": "main", "Bearer key-p2": "feature/parser"},
			semconv.VCSRefHeadTypeKey:                {"Bearer key-p1": "branch", "Bearer key-p2": "branch"},
			semconv.TermaRepositoryWorktreeLinkedKey: {"Bearer key-p1": "false", "Bearer key-p2": "true"},
			semconv.TermaRepositoryWorktreeNameKey:   {"Bearer key-p1": "", "Bearer key-p2": worktree},
			semconv.TermaRepositoryRootKey:           {"Bearer key-p1": "/work/web", "Bearer key-p2": root},
		})
		for name := range r.Stats().Snapshot().Counters {
			if strings.HasPrefix(name, "unclassified.resource/") {
				t.Errorf("the relay's own stamps were counted unclassified: %s", name)
			}
		}
	}
}

// Outside git, and on a detached HEAD, a claim names no repository or branch, and the relay
// stamps none, never the agent's; without tool content it stamps no path or worktree name.
func TestStampCheckoutNamesOnlyWhatTheClaimDoes(t *testing.T) {
	t.Parallel()
	linked := claim.Claim{ProjectID: "p", Root: "/work/web-x", Cwd: "/work/web-x/api", Worktree: "web-x", Remote: "https://github.com/acme/web", Branch: "x"}
	for _, c := range []struct {
		name        string
		ran         claim.Claim
		toolContent bool
		want        map[string]string
	}{
		{"outside git", claim.Claim{ProjectID: "p", Cwd: "/home/dev/notes"}, true,
			map[string]string{semconv.TermaWorkingDirectoryKey: "/home/dev/notes"}},
		{"detached", claim.Claim{ProjectID: "p", Root: "/work/web", Cwd: "/work/web"}, true,
			map[string]string{semconv.TermaRepositoryRootKey: "/work/web", semconv.TermaWorkingDirectoryKey: "/work/web", semconv.TermaRepositoryWorktreeLinkedKey: "false"}},
		{"linked, no tool content", linked, false,
			map[string]string{semconv.VCSRepositoryURLFullKey: "https://github.com/acme/web", semconv.VCSRefHeadNameKey: "x",
				semconv.VCSRefHeadTypeKey: "branch", semconv.TermaRepositoryWorktreeLinkedKey: "true"}},
		{"linked, tool content", linked, true,
			map[string]string{semconv.VCSRepositoryURLFullKey: "https://github.com/acme/web", semconv.VCSRefHeadNameKey: "x",
				semconv.VCSRefHeadTypeKey: "branch", semconv.TermaRepositoryWorktreeLinkedKey: "true",
				semconv.TermaRepositoryRootKey: "/work/web-x", semconv.TermaWorkingDirectoryKey: "/work/web-x/api", semconv.TermaRepositoryWorktreeNameKey: "web-x"}},
	} {
		p := &part{signal: Logs, msg: &logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{
			Resource: &resourcepb.Resource{Attributes: spoofedCheckout()},
		}}}}
		stampCheckout(p, c.ran, c.toolContent)
		res := p.msg.(*logspb.LogsData).ResourceLogs[0].Resource.Attributes
		for _, key := range slices.Concat(checkoutSafeKeys, checkoutContentKeys) {
			if got := attrText(res, key); got != c.want[key] {
				t.Errorf("%s: %s = %q, want %q", c.name, key, got, c.want[key])
			}
		}
	}
}
