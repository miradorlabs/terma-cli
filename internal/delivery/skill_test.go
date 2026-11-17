package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

var update = flag.Bool("update", false, "rewrite the wire fixtures from the current build")

// skillDefinition is a skill's definition as a hook spools it for project p1.
func skillDefinition() spool.Event {
	return spool.Event{Repository: app, Name: semconv.TermaSkillDefinitionEvent, SessionID: "s1", Attrs: map[string]any{
		hookrun.AttrProjectID: "p1", semconv.TermaSkillNameKey: "deploy", semconv.TermaSkillDescriptionKey: "private description",
		semconv.TermaSkillRepositoryURLKey: "https://github.com/acme/app", semconv.TermaSkillPathKey: ".agents/skills/deploy/SKILL.md",
	}}
}

// A skill's definition is content of both kinds: it leaves whole while the policy collects
// prompts and tool content, and not at all otherwise.
func TestOutgoingSendsASkillDefinitionOnlyWhole(t *testing.T) {
	t.Parallel()
	ev := skillDefinition()
	for _, pol := range []config.Policy{
		{Mode: config.ModeRepo, Repositories: listed},
		{Mode: config.ModeRepo, Repositories: listed, IncludePrompts: true},
		{Mode: config.ModeRepo, Repositories: listed, IncludeToolContent: true},
		{Mode: config.ModeRepo, Repositories: listed, IncludePrompts: true, IncludeToolContent: true},
	} {
		out, ok := (Router{ConfigDir: t.TempDir()}).Outgoing(pol, "p1", ev)
		whole := pol.IncludePrompts && pol.IncludeToolContent
		if ok != whole || ok && out.Attrs[semconv.TermaSkillDescriptionKey] != "private description" {
			t.Errorf("prompts %v, tool content %v: %+v, %v", pol.IncludePrompts, pol.IncludeToolContent, out, ok)
		}
	}
}

// A queued definition loses the place of its skill's file once the policy no longer collects
// the repository that place names, the session's own still collected; a plugin's skill keeps
// its marketplace, which no policy lists.
func TestOutgoingDropsASkillLinkThePolicyNoLongerAdmits(t *testing.T) {
	t.Parallel()
	pol := config.Policy{Mode: config.ModeRepo, Repositories: listed, IncludePrompts: true, IncludeToolContent: true}
	for _, tc := range []struct {
		name, origin, url string
		linked            bool
	}{
		{"own", semconv.TermaSkillOriginRepository, "https://github.com/acme/app", true},
		{"own-by-case", semconv.TermaSkillOriginRepository, "https://github.com/Acme/App", true},
		{"dropped", semconv.TermaSkillOriginPersonal, "https://github.com/acme/dotfiles", false},
		{"plugin", semconv.TermaSkillOriginPlugin, "https://github.com/acme/agent-plugins", true},
	} {
		ev := skillDefinition()
		ev.Attrs[semconv.TermaSkillOriginKey], ev.Attrs[semconv.TermaSkillRepositoryURLKey] = tc.origin, tc.url
		out, ok := (Router{ConfigDir: t.TempDir()}).Outgoing(pol, "p1", ev)
		_, hasURL := out.Attrs[semconv.TermaSkillRepositoryURLKey]
		_, hasPath := out.Attrs[semconv.TermaSkillPathKey]
		if !ok || hasURL != tc.linked || hasPath != tc.linked || out.Attrs[semconv.TermaSkillNameKey] != "deploy" {
			t.Errorf("%s: %+v, %v", tc.name, out.Attrs, ok)
		}
		if ev.Attrs[semconv.TermaSkillPathKey] == nil {
			t.Errorf("%s: the queued event was changed", tc.name)
		}
	}
}

// A definition queued while the policy collected everything is withheld once it no longer
// does: nothing of it is sent.
func TestFlushWithholdsASkillDefinitionQueuedBeforeThePolicyChanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sent := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sent++ }))
	defer srv.Close()
	if err := keystore.Set(dir, "p1", testKey, keystore.Hosts{}); err != nil {
		t.Fatal(err)
	}
	s, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(skillDefinition()); err != nil {
		t.Fatal(err)
	}
	now := config.Policy{Mode: config.ModeRepo, Repositories: listed, TeamID: "p1", IncludePrompts: true, FetchedAt: time.Now()}
	r := Router{ConfigDir: dir, OTLPPinned: true, Policy: func(context.Context, *config.Config, string) (config.Policy, error) { return now, nil }}
	res := r.Flush(t.Context(), s, &config.Config{OTLPURL: srv.URL}, true, 0)
	if res.Withheld != 1 || res.Sent != 0 || sent != 0 || res.Err != nil {
		t.Fatalf("Flush = %+v, %d requests", res, sent)
	}
}

// terma.skill.definition as the platform receives it: the OTLP/JSON body the sender posts,
// indented. The platform keeps a copy of testdata/terma_skill_definition_v1.json; a change
// here is a change to the contract (-update rewrites it).
func TestSkillDefinitionWireContract(t *testing.T) {
	t.Parallel()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	at := time.Date(2026, 11, 17, 14, 54, 8, 0, time.UTC)
	widgets := config.Repository{Origin: "github.com/acme/widgets"}
	events := []spool.Event{
		{Time: at, Repository: widgets, Name: semconv.TermaSkillDefinitionEvent, SessionID: "00000000-0000-4000-8000-00000000c1a0", Attrs: map[string]any{
			hookrun.AttrProjectID: "proj_acme", semconv.GenAIMainAgentNameKey: "claude-code", semconv.TermaEvidenceSourceKey: "claude_transcript",
			semconv.TermaSkillNameKey: "tools:release-notes", semconv.TermaSkillDescriptionKey: "Drafts release notes from merged pull requests. Use before cutting a release.",
			semconv.TermaSkillOriginKey: semconv.TermaSkillOriginPlugin, semconv.TermaSkillPluginNameKey: "tools", semconv.TermaSkillMarketplaceNameKey: "acme-tools",
			semconv.TermaSkillRepositoryURLKey: "https://github.com/acme/agent-plugins", semconv.TermaSkillPathKey: "plugins/tools/skills/release-notes/SKILL.md",
		}},
		{Time: at.Add(time.Second), Repository: widgets, Name: semconv.TermaSkillDefinitionEvent, SessionID: "019947ab-1234-7000-8000-123456789abc", Attrs: map[string]any{
			hookrun.AttrProjectID: "proj_acme", semconv.GenAIMainAgentNameKey: "codex", semconv.TermaEvidenceSourceKey: "codex_hook",
			semconv.TermaSkillNameKey: "hello-codex", semconv.TermaSkillDescriptionKey: "Replies with a fixed greeting. Use when the user asks for the hello-codex greeting.",
			semconv.TermaSkillOriginKey:        semconv.TermaSkillOriginRepository,
			semconv.TermaSkillRepositoryURLKey: "https://github.com/acme/widgets", semconv.TermaSkillPathKey: ".agents/skills/hello-codex/SKILL.md",
		}},
	}
	sender := &otlpSender{Endpoint: srv.URL, APIKey: "k", ProjectID: "proj_acme", Version: "1.2.3", HTTP: srv.Client()}
	if _, err := sender.Send(t.Context(), wire(events)); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := json.Indent(&got, body, "", "  "); err != nil {
		t.Fatal(err)
	}
	got.WriteByte('\n')
	path := filepath.Join("testdata", "terma_skill_definition_v1.json")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no fixture (%v): write it with -update", err)
	}
	if !bytes.Equal(want, got.Bytes()) {
		t.Errorf("the wire form changed: it is the platform's contract.\n--- fixture\n%s\n--- now\n%s", want, got.String())
	}
	if strings.Contains(got.String(), hookrun.AttrProjectID) {
		t.Error("the routing key left")
	}
}
