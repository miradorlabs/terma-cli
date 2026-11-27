package claude

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// clearClaudeRoute leaves no base URL or provider switch from the developer's environment.
func clearClaudeRoute(t *testing.T) {
	t.Helper()
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("CLAUDE_CODE_USE_GATEWAY", "")
	for _, k := range claudeCloudSwitches {
		t.Setenv(k, "")
	}
}

func TestClaudeServingBackend(t *testing.T) {
	const proxy, anthropic = "https://cliproxy.example.com", "https://api.anthropic.com"
	env := func(url string) string { return `{"env":{"ANTHROPIC_BASE_URL":"` + url + `"}}` }
	for _, tc := range []struct {
		name               string
		vars               map[string]string
		user, project, own string // settings files: the user's, the project's, the project's local
		want               string
	}{
		{name: "unset"},
		{name: "proxy in the environment", vars: map[string]string{"ANTHROPIC_BASE_URL": proxy}, want: semconv.TermaServingBackendPrivate},
		{name: "local server in the environment", vars: map[string]string{"ANTHROPIC_BASE_URL": "http://localhost:4000"}, want: semconv.TermaServingBackendPrivate},
		{name: "anthropic in the environment", vars: map[string]string{"ANTHROPIC_BASE_URL": anthropic}},
		{name: "empty in the environment", vars: map[string]string{"ANTHROPIC_BASE_URL": " "}},
		{name: "proxy in the user's settings", user: env(proxy), want: semconv.TermaServingBackendPrivate},
		{name: "empty environment falls through to settings", vars: map[string]string{"ANTHROPIC_BASE_URL": ""}, user: env(proxy), want: semconv.TermaServingBackendPrivate},
		{name: "environment outranks settings", vars: map[string]string{"ANTHROPIC_BASE_URL": anthropic}, user: env(proxy)},
		{name: "project outranks user", user: env(proxy), project: env(anthropic)},
		{name: "local outranks project", project: env(anthropic), own: env(proxy), want: semconv.TermaServingBackendPrivate},
		{name: "empty in settings falls through", own: env(""), user: env(proxy), want: semconv.TermaServingBackendPrivate},
		{name: "settings without env", user: `{"model":"opus"}`},
		{name: "malformed settings", user: `{"env":`},
		{name: "bedrock ignores the base URL", vars: map[string]string{"ANTHROPIC_BASE_URL": proxy, "CLAUDE_CODE_USE_BEDROCK": "1"}},
		{name: "vertex switch in settings", user: `{"env":{"ANTHROPIC_BASE_URL":"` + proxy + `","CLAUDE_CODE_USE_VERTEX":"true"}}`},
		{name: "gateway switch beside a cloud", vars: map[string]string{"ANTHROPIC_BASE_URL": proxy, "CLAUDE_CODE_USE_FOUNDRY": "1", "CLAUDE_CODE_USE_GATEWAY": "1"}, want: semconv.TermaServingBackendPrivate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearClaudeRoute(t)
			config := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", config)
			for k, v := range tc.vars {
				t.Setenv(k, v)
			}
			root := t.TempDir()
			for path, data := range map[string]string{
				filepath.Join(config, "settings.json"):                tc.user,
				filepath.Join(root, ".claude", "settings.json"):       tc.project,
				filepath.Join(root, ".claude", "settings.local.json"): tc.own,
			} {
				if data != "" {
					writeEvidenceFile(t, path, data)
				}
			}
			if got := claudeServingBackend(root); got != tc.want {
				t.Fatalf("claudeServingBackend = %q, want %q", got, tc.want)
			}
		})
	}
}

// A stop through a proxy marks each call's response private, and never names the host.
func TestClaudeStopMarksResponsesThroughAProxy(t *testing.T) {
	env := titleEnv(t)
	clearClaudeRoute(t)
	t.Setenv("ANTHROPIC_BASE_URL", "https://cliproxy.example.com")
	stopClaude(t, env, transcript(t, t.TempDir(), assistantRecord("req_a", "2026-10-28T03:31:45Z", 76)))
	got := spooledResponses(t, env.Spool)
	if len(got) != 1 || got[0].Attrs[semconv.TermaServingBackendKey] != semconv.TermaServingBackendPrivate {
		t.Fatalf("got %+v, want one private response", got)
	}
	if b, _ := json.Marshal(got[0]); strings.Contains(string(b), "cliproxy") {
		t.Fatalf("response names the host: %s", b)
	}
}
