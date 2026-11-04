package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// Each route Claude Code can be configured with maps to one backend, or to none.
func TestRouteBackend(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		env    map[string]string
		stored bool
		want   string
	}{
		{"bedrock", map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}, false, semconv.TermaServingBackendAwsBedrock},
		{"vertex", map[string]string{"CLAUDE_CODE_USE_VERTEX": "true"}, false, semconv.TermaServingBackendGoogleVertex},
		{"foundry", map[string]string{"CLAUDE_CODE_USE_FOUNDRY": "yes"}, false, semconv.TermaServingBackendAzureFoundry},
		{"bedrock before vertex", map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "CLAUDE_CODE_USE_VERTEX": "1"}, false, semconv.TermaServingBackendAwsBedrock},
		{"foundry before vertex", map[string]string{"CLAUDE_CODE_USE_VERTEX": "1", "CLAUDE_CODE_USE_FOUNDRY": "1"}, false, semconv.TermaServingBackendAzureFoundry},
		{"mantle", map[string]string{"CLAUDE_CODE_USE_MANTLE": "1"}, true, semconv.TermaServingBackendAwsBedrock},
		{"mantle before vertex", map[string]string{"CLAUDE_CODE_USE_MANTLE": "1", "CLAUDE_CODE_USE_VERTEX": "1"}, false, semconv.TermaServingBackendAwsBedrock},
		{"mantle through a gateway", map[string]string{"CLAUDE_CODE_USE_MANTLE": "1", "ANTHROPIC_BEDROCK_MANTLE_BASE_URL": "https://litellm.corp.example"}, false, semconv.TermaServingBackendPrivate},
		{"mantle at its own endpoint", map[string]string{"CLAUDE_CODE_USE_MANTLE": "1", "ANTHROPIC_BEDROCK_MANTLE_BASE_URL": "https://bedrock-mantle.us-east-1.api.aws/anthropic"}, false, semconv.TermaServingBackendAwsBedrock},
		{"cloud switch over a gateway base URL", map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "ANTHROPIC_BASE_URL": "https://llm.corp.example"}, false, semconv.TermaServingBackendAwsBedrock},
		{"bedrock through a gateway", map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "ANTHROPIC_BEDROCK_BASE_URL": "https://litellm.corp.example/bedrock"}, false, semconv.TermaServingBackendPrivate},
		{"vertex through a gateway", map[string]string{"CLAUDE_CODE_USE_VERTEX": "1", "ANTHROPIC_VERTEX_BASE_URL": "http://localhost:4000/vertex_ai/v1"}, false, semconv.TermaServingBackendPrivate},
		{"foundry through a gateway", map[string]string{"CLAUDE_CODE_USE_FOUNDRY": "1", "ANTHROPIC_FOUNDRY_BASE_URL": "https://apim.azure-api.net/anthropic"}, false, semconv.TermaServingBackendPrivate},
		{"bedrock through its VPC endpoint", map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "ANTHROPIC_BEDROCK_BASE_URL": "https://vpce-0abc.bedrock-runtime.us-east-1.vpce.amazonaws.com"}, false, semconv.TermaServingBackendAwsBedrock},
		{"bedrock through the AWS SDK's endpoint", map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "AWS_ENDPOINT_URL_BEDROCK_RUNTIME": "http://localhost:4566"}, false, semconv.TermaServingBackendPrivate},
		{"bedrock pointed at Vertex's host", map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "ANTHROPIC_BEDROCK_BASE_URL": "https://aiplatform.googleapis.com"}, false, semconv.TermaServingBackendPrivate},
		{"Claude Platform on AWS", map[string]string{"CLAUDE_CODE_USE_ANTHROPIC_AWS": "1", "ANTHROPIC_AWS_BASE_URL": "https://llm.corp.example"}, true, ""},
		{"Claude Platform on Google Cloud before vertex", map[string]string{"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD": "1", "CLAUDE_CODE_USE_VERTEX": "1"}, true, ""},
		{"switch spelled on", map[string]string{"CLAUDE_CODE_USE_BEDROCK": "on"}, true, semconv.TermaServingBackendAwsBedrock},
		{"bedrock at its own regional endpoint", map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1", "ANTHROPIC_BEDROCK_BASE_URL": "https://bedrock-runtime.eu-west-1.amazonaws.com"}, false, semconv.TermaServingBackendAwsBedrock},
		{"another provider's base URL", map[string]string{"CLAUDE_CODE_USE_VERTEX": "1", "ANTHROPIC_BEDROCK_BASE_URL": "https://litellm.corp.example"}, false, semconv.TermaServingBackendGoogleVertex},
		{"cloud switch over a stored login", map[string]string{"CLAUDE_CODE_USE_VERTEX": "1"}, true, semconv.TermaServingBackendGoogleVertex},
		{"switch off", map[string]string{"CLAUDE_CODE_USE_BEDROCK": "0", "ANTHROPIC_API_KEY": "k"}, false, semconv.TermaServingBackendAnthropic},
		{"anthropic base URL", map[string]string{"ANTHROPIC_BASE_URL": "https://api.anthropic.com"}, false, semconv.TermaServingBackendAnthropic},
		{"bedrock base URL", map[string]string{"ANTHROPIC_BASE_URL": "https://bedrock-runtime.us-east-1.amazonaws.com"}, false, semconv.TermaServingBackendAwsBedrock},
		{"gateway base URL with a key", map[string]string{"ANTHROPIC_BASE_URL": "https://llm.corp.example", "ANTHROPIC_API_KEY": "k"}, false, semconv.TermaServingBackendPrivate},
		{"local base URL with a stored login", map[string]string{"ANTHROPIC_BASE_URL": "http://localhost:4000"}, true, semconv.TermaServingBackendPrivate},
		{"gateway base URL without a credential", map[string]string{"ANTHROPIC_BASE_URL": "https://llm.corp.example"}, false, semconv.TermaServingBackendPrivate},
		{"base URL with no host", map[string]string{"ANTHROPIC_BASE_URL": "api.anthropic.com", "ANTHROPIC_API_KEY": "k"}, false, ""},
		{"api key", map[string]string{"ANTHROPIC_API_KEY": "k"}, false, semconv.TermaServingBackendAnthropic},
		{"auth token", map[string]string{"ANTHROPIC_AUTH_TOKEN": "t"}, false, semconv.TermaServingBackendAnthropic},
		{"oauth token", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "t"}, false, semconv.TermaServingBackendAnthropic},
		{"stored login or apiKeyHelper", nil, true, semconv.TermaServingBackendAnthropic},
		{"no visible credential", nil, false, ""},
	} {
		lookup := func(key string) string { return tc.env[key] }
		if got := routeBackend(lookup, tc.stored); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Only the hosts a backend serves its API from are named; every other host is private, and a
// base URL with no host says nothing.
func TestBaseURLBackend(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"https://api.anthropic.com":                                      semconv.TermaServingBackendAnthropic,
		"https://API.Anthropic.com.:443/v1":                              semconv.TermaServingBackendAnthropic,
		"https://bedrock-runtime.eu-west-1.amazonaws.com":                semconv.TermaServingBackendAwsBedrock,
		"https://bedrock-runtime-fips.us-gov-west-1.amazonaws.com":       semconv.TermaServingBackendAwsBedrock,
		"https://bedrock-runtime.cn-north-1.amazonaws.com.cn":            semconv.TermaServingBackendAwsBedrock,
		"https://bedrock-mantle.eu-west-1.api.aws/anthropic":             semconv.TermaServingBackendAwsBedrock,
		"https://us-east5-aiplatform.googleapis.com/v1":                  semconv.TermaServingBackendGoogleVertex,
		"https://aiplatform.googleapis.com":                              semconv.TermaServingBackendGoogleVertex,
		"https://my-resource.services.ai.azure.com/anthropic":            semconv.TermaServingBackendAzureFoundry,
		"https://bedrock.us-east-1.amazonaws.com":                        semconv.TermaServingBackendAwsBedrock,
		"https://vpce-0abc.bedrock-runtime.us-east-1.vpce.amazonaws.com": semconv.TermaServingBackendAwsBedrock,
		"https://bedrock-runtime.eu-central-1.amazonaws.eu":              semconv.TermaServingBackendAwsBedrock,
		"https://bedrock-runtime.us-east-1.amazonaws.com.evil.example":   semconv.TermaServingBackendPrivate,
		"https://aiplatform.europe-west4.rep.googleapis.com":             semconv.TermaServingBackendGoogleVertex,
		"https://my-psc.p.googleapis.com":                                semconv.TermaServingBackendGoogleVertex,
		"https://my-resource.cognitiveservices.azure.com":                semconv.TermaServingBackendAzureFoundry,
		"https://my-resource.services.ai.azure.us":                       semconv.TermaServingBackendAzureFoundry,
		"https://apim.azure-api.net/anthropic":                           semconv.TermaServingBackendPrivate,
		"https://api.anthropic.com.evil.example":                         semconv.TermaServingBackendPrivate,
		"https://console.anthropic.com":                                  semconv.TermaServingBackendPrivate,
		"https://openrouter.ai/api":                                      semconv.TermaServingBackendPrivate,
		"https://gateway.ai.cloudflare.com/v1/acct/gw/anthropic":         semconv.TermaServingBackendPrivate,
		"http://localhost:4000":                                          semconv.TermaServingBackendPrivate,
		"api.anthropic.com":                                              "",
		"://":                                                            "",
		"":                                                               "",
	} {
		if got := baseURLBackend(raw); got != want {
			t.Errorf("%q: got %q, want %q", raw, got, want)
		}
	}
}

// The settings files' env blocks fill what the hook's environment leaves unset, the project's
// local file first; an apiKeyHelper or the stored login is a first-party credential.
func TestServingBackendReadsSettings(t *testing.T) {
	env := newFundingEnv(t)
	userDir := os.Getenv("CLAUDE_CONFIG_DIR")
	projectDir := filepath.Join(env.Cwd, ".claude")
	if got := servingBackend(env.Cwd); got != "" {
		t.Fatalf("no route: got %q", got)
	}

	writeRealClaudeAccount(t)
	if got := servingBackend(env.Cwd); got != semconv.TermaServingBackendAnthropic {
		t.Fatalf("stored login: got %q, want anthropic", got)
	}

	hookruntest.WriteFile(t, userDir, "settings.json", `{"env":{"ANTHROPIC_BASE_URL":"https://llm.corp.example"}}`)
	if got := servingBackend(env.Cwd); got != semconv.TermaServingBackendPrivate {
		t.Fatalf("user settings gateway: got %q", got)
	}
	hookruntest.WriteFile(t, projectDir, "settings.json", `{"env":{"ANTHROPIC_BASE_URL":"https://other.corp.example"}}`)
	hookruntest.WriteFile(t, projectDir, "settings.local.json", `{"env":{"ANTHROPIC_BASE_URL":"https://api.anthropic.com"}}`)
	if got := servingBackend(env.Cwd); got != semconv.TermaServingBackendAnthropic {
		t.Fatalf("local settings over project and user: got %q, want anthropic", got)
	}
	hookruntest.WriteFile(t, userDir, "settings.json", `{"env":{"CLAUDE_CODE_USE_VERTEX":"1"}}`)
	if got := servingBackend(env.Cwd); got != semconv.TermaServingBackendGoogleVertex {
		t.Fatalf("vertex switch in user settings: got %q", got)
	}
	t.Setenv("CLAUDE_CODE_USE_VERTEX", "0")
	if got := servingBackend(env.Cwd); got != semconv.TermaServingBackendAnthropic {
		t.Fatalf("the environment over settings: got %q, want anthropic", got)
	}

	if err := os.Remove(filepath.Join(userDir, ".claude.json")); err != nil {
		t.Fatal(err)
	}
	hookruntest.WriteFile(t, projectDir, "settings.local.json", `{"apiKeyHelper":"/usr/local/bin/get-key"}`)
	hookruntest.WriteFile(t, projectDir, "settings.json", `{}`)
	if got := servingBackend(env.Cwd); got != semconv.TermaServingBackendAnthropic {
		t.Fatalf("apiKeyHelper: got %q, want anthropic", got)
	}

	// A host managing the provider has Claude Code ignore the settings files' route.
	hookruntest.WriteFile(t, projectDir, "settings.json", `{"env":{"CLAUDE_CODE_USE_BEDROCK":"1"}}`)
	t.Setenv("CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST", "1")
	if got := servingBackend(env.Cwd); got != semconv.TermaServingBackendAnthropic {
		t.Fatalf("host-managed provider: got %q, want the settings' switch ignored", got)
	}
	t.Setenv("CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST", "")

	// A settings file terma cannot read may hide the route: nothing is reported.
	hookruntest.WriteFile(t, projectDir, "settings.json", `{`)
	writeRealClaudeAccount(t)
	if got := servingBackend(env.Cwd); got != "" {
		t.Fatalf("unreadable settings: got %q, want nothing", got)
	}
}

// Each call's response names its backend; an unrecognised base URL is private, and its host
// never leaves.
func TestClaudeStopSpoolsServingBackend(t *testing.T) {
	env := titleEnv(t)
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	path := transcript(t, t.TempDir(), assistantRecord("req_a", "2026-10-28T03:31:45Z", 76))
	stopClaude(t, env, path)
	got := spooledResponses(t, env.Spool)
	if len(got) != 1 || got[0].Attrs[semconv.TermaServingBackendKey] != semconv.TermaServingBackendAwsBedrock {
		t.Fatalf("bedrock: got %+v", got)
	}

	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://llm.corp.example")
	t.Setenv("ANTHROPIC_API_KEY", "k")
	appendTranscript(t, path, assistantRecord("req_b", "2026-10-28T03:32:00Z", 12)+"\n")
	stopClaude(t, env, path)
	got = spooledResponses(t, env.Spool)
	raw, _ := json.Marshal(got)
	if len(got) != 1 || got[0].Attrs[semconv.TermaServingBackendKey] != semconv.TermaServingBackendPrivate || strings.Contains(string(raw), "corp.example") {
		t.Fatalf("gateway: got %s, want private and no host", raw)
	}
}
