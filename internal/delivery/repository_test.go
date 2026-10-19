package delivery

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Every event queued in a repository leaves naming it, the ones spooled without it (by a
// terma that did not stamp it yet) too; one that names its own keeps it, and one from
// outside git names none.
func TestEveryEventLeavesNamingItsRepository(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	if err := keystore.Set(dir, "p1", testKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	s, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const own = "https://github.com/acme/app"
	attrs := func(extra ...string) map[string]any {
		m := map[string]any{hookrun.AttrProjectID: "p1", semconv.GenAIMainAgentNameKey: "fake"}
		for i := 0; i < len(extra); i += 2 {
			m[extra[i]] = extra[i+1]
		}
		return m
	}
	for _, e := range []spool.Event{
		{Repository: app, Name: semconv.TermaSessionStartEvent, SessionID: "start", Attrs: attrs()},
		{Repository: app, Name: semconv.TermaSessionTitleEvent, SessionID: "title", Attrs: attrs()},
		{Repository: app, Name: semconv.TermaAssistantMessageEvent, SessionID: "reply", Attrs: attrs()},
		{Repository: app, Name: semconv.TermaToolCallEvent, SessionID: "tool", Attrs: attrs()},
		{Repository: app, Name: semconv.TermaSubagentStartEvent, SessionID: "subagent", Attrs: attrs()},
		{Repository: app, Name: semconv.TermaSessionEndEvent, SessionID: "end", Attrs: attrs()},
		{Repository: app, Name: semconv.TermaFilesTouchedEvent, SessionID: "touch", Attrs: attrs(semconv.VCSRepositoryURLFullKey, "https://github.com/acme/other")},
		{Name: semconv.TermaSessionStartEvent, SessionID: "outside-git", Global: true, Attrs: attrs()},
	} {
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	pol := config.Policy{Mode: config.ModeGlobal, DefaultProjectID: "p1", TeamID: "p1", IncludePrompts: true, IncludeToolContent: true, FetchedAt: time.Now()}
	r := Router{ConfigDir: dir, OTLPPinned: true,
		Policy:  func(context.Context, *config.Config, string) (config.Policy, error) { return pol, nil },
		Consent: func(string, hookrun.Consent) bool { return true },
	}
	if res := r.Flush(t.Context(), s, &config.Config{OTLPURL: srv.URL}, true, 0); res.Sent != 8 || res.Err != nil {
		t.Fatalf("Flush = %+v", res)
	}
	var payload struct {
		ResourceLogs []struct {
			ScopeLogs []struct {
				LogRecords []struct {
					Attributes []otlpAttr `json:"attributes"`
				} `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	got := map[string]string{}
	for _, rec := range payload.ResourceLogs[0].ScopeLogs[0].LogRecords {
		var session, repo string
		for _, a := range rec.Attributes {
			switch a.Key {
			case semconv.SessionIDKey:
				session = *a.Value.String
			case semconv.VCSRepositoryURLFullKey:
				repo = *a.Value.String
			}
		}
		got[session] = repo
	}
	for _, session := range []string{"start", "title", "reply", "tool", "subagent", "end"} {
		if got[session] != own {
			t.Errorf("%s left naming %q, want %q", session, got[session], own)
		}
	}
	if got["touch"] != "https://github.com/acme/other" {
		t.Errorf("a touch's own repository was replaced: %q", got["touch"])
	}
	if repo, sent := got["outside-git"]; !sent || repo != "" {
		t.Errorf("an event from outside git left naming %q (sent %v)", repo, sent)
	}
}

// The URL delivery stamps keys back, on the server, to exactly the repository the event was
// admitted under, and to the key of the origin a touch, commit or push stamps itself.
func TestRepositoryURLKeysBackToItsAdmission(t *testing.T) {
	t.Parallel()
	for origin, stamp := range map[string]string{
		"git@github.com:acme/repo.git":                         "https://github.com/acme/repo",
		"git@GitHub.com:Acme/Repo":                             "https://github.com/Acme/Repo",
		"https://alex:tok@GitHub.com:443/acme/repo.git":        "https://github.com/acme/repo",
		"https://github.com/acme/repo.git/":                    "https://github.com/acme/repo",
		"ssh://git@gitlab.example.com:2222/group/sub/repo.git": "https://gitlab.example.com/group/sub/repo",
		"git://git.example.com/acme/repo.git":                  "https://git.example.com/acme/repo",
		"git@github-work:acme/api.git":                         "https://github-work/acme/api",
		"https://dev.azure.com/org/Project%20X/_git/repo":      "https://dev.azure.com/org/Project%20X/_git/repo",
		"git@ssh.dev.azure.com:v3/org/project/repo":            "https://ssh.dev.azure.com/v3/org/project/repo",
		"https://github.com/acme/./app":                        "https://github.com/acme/./app",
		"https://git.example.com/acme/repo.git.git.git":        "https://git.example.com/acme/repo.git.git",
		"https://github.com/acme/app?tab=readme#top":           "https://github.com/acme/app",
	} {
		key := gitx.RepositoryID(origin)
		got := repositoryURL(config.Repository{Origin: key})
		if got != stamp {
			t.Errorf("origin %q (key %q): stamp %q, want %q", origin, key, got, stamp)
		}
		if serverKey(got) != key {
			t.Errorf("origin %q: stamp %q keys to %q, admitted as %q", origin, got, serverKey(got), key)
		}
		if own := gitx.NormalizeRemote(origin); serverKey(own) != serverKey(got) {
			t.Errorf("origin %q: a touch's %q keys to %q, delivery's %q to %q", origin, own, serverKey(own), got, serverKey(got))
		}
	}
	if got := repositoryURL(config.Repository{}); got != "" {
		t.Errorf("outside git: %q", got)
	}
}

// serverKey is mirador-platform's gitactivity.RepoKeyFromURL (#2259), the key the server
// reads from vcs.repository.url.full, but for its UTF-8 check, which no key here fails.
func serverKey(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return ""
	}
	switch u.Scheme {
	case "http", "https", "ssh", "git":
	default:
		return ""
	}
	p := strings.Trim(strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"), "/")
	if segments := strings.Split(p, "/"); len(segments) < 2 || slices.Contains(segments, "") {
		return ""
	}
	return strings.ToLower(u.Hostname()) + "/" + p
}
