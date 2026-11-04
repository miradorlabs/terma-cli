package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Only OpenAI's and Azure OpenAI's hosts are named; every other base URL is unknown.
func TestCodexBaseURLBackend(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"https://api.openai.com/v1":                              semconv.TermaServingBackendOpenai,
		"https://API.OpenAI.com.:443/v1":                         semconv.TermaServingBackendOpenai,
		"https://my-resource.openai.azure.com/openai":            semconv.TermaServingBackendAzureOpenai,
		"https://my-resource.openai.azure.com/openai/v1":         semconv.TermaServingBackendAzureOpenai,
		"https://my-project.services.ai.azure.com/openai/v1":     semconv.TermaServingBackendAzureOpenai,
		"https://my-resource.cognitiveservices.azure.com/openai": semconv.TermaServingBackendAzureOpenai,
		"https://api.openai.com.evil.example":                    "",
		"https://openai.azure.com.evil.example":                  "",
		"https://chatgpt.com/backend-api/codex":                  "",
		"https://openrouter.ai/api/v1":                           "",
		"https://my-gateway.azure-api.net/openai":                "",
		"https://gateway.ai.cloudflare.com/v1/acct/gw/openai":    "",
		"http://localhost:11434/v1":                              "",
		"api.openai.com":                                         "",
		"":                                                       "",
	} {
		if got := codexBaseURLBackend(raw); got != want {
			t.Errorf("%q: got %q, want %q", raw, got, want)
		}
	}
}

// The session's provider id maps through where config.toml sends it: the built-in openai
// provider is OpenAI unless openai_base_url or OPENAI_BASE_URL moves it, and a custom id is
// only what its base_url's host says.
func TestCodexRoutesBackend(t *testing.T) {
	const providers = `
[model_providers.azure]
name = "Azure"
base_url = "https://my-resource.openai.azure.com/openai"

[model_providers.foundry]
base_url = "https://my-project.services.ai.azure.com/openai/v1"

[model_providers.direct]
base_url = "https://api.openai.com/v1"

[model_providers.gateway]
base_url = "https://llm.corp.example/v1"

[model_providers.openai]
base_url = "https://llm.corp.example/v1"
`
	for _, tc := range []struct {
		name, config, env, provider, want string
	}{
		{"openai without a config file", "", "", "openai", semconv.TermaServingBackendOpenai},
		{"openai with providers defined", providers, "", "openai", semconv.TermaServingBackendOpenai},
		{"openai_base_url to a gateway", `openai_base_url = "https://llm.corp.example/v1"`, "", "openai", ""},
		{"openai_base_url to OpenAI", `openai_base_url = "https://api.openai.com/v1"`, "", "openai", semconv.TermaServingBackendOpenai},
		{"openai_base_url to Azure", `openai_base_url = "https://r.openai.azure.com/openai/v1"`, "", "openai", semconv.TermaServingBackendAzureOpenai},
		{"OPENAI_BASE_URL to a gateway", "", "https://llm.corp.example/v1", "openai", ""},
		{"OPENAI_BASE_URL to Azure", "", "https://r.openai.azure.com/openai/v1", "openai", semconv.TermaServingBackendAzureOpenai},
		{"openai_base_url over OPENAI_BASE_URL", `openai_base_url = "https://api.openai.com/v1"`, "https://llm.corp.example/v1", "openai", semconv.TermaServingBackendOpenai},
		{"azure", providers, "", "azure", semconv.TermaServingBackendAzureOpenai},
		{"foundry", providers, "", "foundry", semconv.TermaServingBackendAzureOpenai},
		{"custom id to OpenAI", providers, "", "direct", semconv.TermaServingBackendOpenai},
		{"custom id to a gateway", providers, "", "gateway", ""},
		{"undefined id (a -c override)", providers, "", "telemetry_fixture", ""},
		{"ollama", "", "", "ollama", ""},
		{"oss", "", "", "oss", ""},
		{"no provider", providers, "", "", ""},
		{"unparsable config", "model_providers = [", "", "openai", ""},
		{"mistyped openai_base_url", `openai_base_url = 1`, "", "openai", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			t.Setenv("OPENAI_BASE_URL", tc.env)
			if tc.config != "" {
				hookruntest.WriteFile(t, home, codexConfigFile, tc.config)
			}
			if got := readCodexRoutes().backend(tc.provider); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// providerRollout writes testdata/codex_rollout_provider.jsonl, a real codex-cli 0.164.0
// rollout trimmed to one turn on the openai provider, and returns its path.
func providerRollout(t *testing.T, cwd string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "codex_rollout_provider.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	q, _ := json.Marshal(cwd)
	rollout := strings.NewReplacer("{{ID}}", replySession, `"{{CWD}}"`, string(q)).Replace(string(b))
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "10", "30", "rollout-2026-10-30T05-17-43-"+replySession+".jsonl")
	hookruntest.WriteFile(t, filepath.Dir(path), filepath.Base(path), rollout)
	return path
}

// Every quota observation names the session's backend; a resume onto another provider moves
// it from there on, across reads, and a provider config.toml cannot place sends no key at all.
func TestCodexQuotaCarriesServingBackend(t *testing.T) {
	env := fundingEnv(t)
	hookruntest.WriteFile(t, os.Getenv("CODEX_HOME"), codexConfigFile, `
[model_providers.azure]
base_url = "https://my-resource.openai.azure.com/openai"

[model_providers.gateway]
base_url = "https://llm.corp.example/v1"
`)
	path := providerRollout(t, env.Cwd)
	stopOn := func() []spool.Event {
		in, _ := json.Marshal(map[string]any{"session_id": replySession, "cwd": env.Cwd, "transcript_path": path})
		env.Stdin = strings.NewReader(string(in))
		if err := stop(context.Background(), env); err != nil {
			t.Fatal(err)
		}
		return hookruntest.Named(hookruntest.Spooled(t, env.Spool), semconv.TermaSessionQuotaEvent)
	}
	quotas := stopOn()
	if len(quotas) != 1 || quotas[0].Attrs[semconv.TermaServingBackendKey] != semconv.TermaServingBackendOpenai ||
		quotas[0].Attrs[semconv.TermaAccountPlanTypeKey] != "self_serve_business_prolite" {
		t.Fatalf("real openai session: %+v", quotas)
	}

	resumed := func(provider string) string {
		q, _ := json.Marshal(env.Cwd)
		return `{"timestamp":"2026-10-30T05:00:00Z","type":"event_msg","payload":{"type":"thread_settings_applied","thread_id":"` + replySession +
			`","thread_settings":{"model":"gpt-6-astra","model_provider_id":"` + provider + `","cwd":` + string(q) + `}}}` + "\n" +
			`{"timestamp":"2026-10-30T05:00:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-` + provider + `"}}` + "\n" +
			`{"timestamp":"2026-10-30T05:00:02Z","type":"event_msg","payload":{"type":"token_count","rate_limits":null}}` + "\n"
	}
	appendRollout := func(text string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		if _, err := f.WriteString(text); err != nil {
			t.Fatal(err)
		}
	}
	appendRollout(resumed("azure"))
	quotas = stopOn()
	if len(quotas) != 1 || quotas[0].Attrs[semconv.TermaServingBackendKey] != semconv.TermaServingBackendAzureOpenai {
		t.Fatalf("resumed on azure: %+v", quotas)
	}
	// A later read starts past the resume and still knows the provider.
	appendRollout(`{"timestamp":"2026-10-30T05:00:03Z","type":"event_msg","payload":{"type":"token_count","rate_limits":null}}` + "\n")
	quotas = stopOn()
	if len(quotas) != 1 || quotas[0].Attrs[semconv.TermaServingBackendKey] != semconv.TermaServingBackendAzureOpenai {
		t.Fatalf("next read on azure: %+v", quotas)
	}

	appendRollout(resumed("gateway"))
	quotas = stopOn()
	raw, _ := json.Marshal(quotas)
	if len(quotas) != 1 || quotas[0].Attrs[semconv.TermaServingBackendKey] != nil || strings.Contains(string(raw), "corp.example") || strings.Contains(string(raw), `"gateway"`) {
		t.Fatalf("resumed on a gateway: %s, want no backend, host or provider id", raw)
	}
}
