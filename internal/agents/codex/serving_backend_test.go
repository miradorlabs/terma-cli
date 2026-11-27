package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

const servingConfig = `model_provider = "cliproxyapi"
openai_base_url = ""

[model_providers.cliproxyapi]
name = "CLIProxyAPI"
base_url = "https://cliproxy.example.com/v1"
wire_api = "responses"

[model_providers.azure]
base_url = "https://my-res.openai.azure.com/openai"

[model_providers.blank]
base_url = ""

[model_providers.local]
base_url = "http://localhost:11434/v1"
`

func TestCodexServingBackend(t *testing.T) {
	t.Parallel()
	const private = semconv.TermaServingBackendPrivate
	providers := func(openAI string) codexProviders {
		return codexProviders{openAIBaseURL: openAI, baseURLs: map[string]string{
			"cliproxyapi": "https://cliproxy.example.com/v1", "azure": "https://my-res.openai.azure.com/openai",
			"openai-proxy": "https://api.openai.com/v1", "blank": "", "local": "http://localhost:11434/v1",
		}}
	}
	for _, tc := range []struct {
		provider, openAI, want string
	}{
		{provider: ""},
		{provider: "openai"},
		{provider: "openai", openAI: "https://api.openai.com/v1"},
		{provider: "openai", openAI: "https://cliproxy.example.com/v1", want: private},
		{provider: "cliproxyapi", want: private},
		{provider: "local", want: private},
		{provider: "ollama", want: private},
		{provider: "lmstudio", want: private},
		{provider: "azure"},
		{provider: "openai-proxy"},
		{provider: "amazon-bedrock"},
		{provider: "blank"},
		{provider: "undefined"},
	} {
		if got := codexServingBackend(tc.provider, providers(tc.openAI)); got != tc.want {
			t.Errorf("codexServingBackend(%q, openai_base_url %q) = %q, want %q", tc.provider, tc.openAI, got, tc.want)
		}
	}
}

func TestReadCodexProviders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if got := readCodexProviders(); got.baseURLs != nil {
		t.Fatalf("no config.toml: %+v", got)
	}
	writeEvidenceFile(t, filepath.Join(home, "config.toml"), servingConfig)
	got := readCodexProviders()
	if got.baseURLs["cliproxyapi"] != "https://cliproxy.example.com/v1" || got.baseURLs["azure"] != "https://my-res.openai.azure.com/openai" {
		t.Fatalf("got %+v", got)
	}
}

// Each quota record, the unavailable ones included, carries the backend of the provider the
// thread is on at that point of the rollout: the session's first, then the one a resume moved it to.
func TestCodexQuotaRecordsCarryTheServingBackend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	writeEvidenceFile(t, filepath.Join(home, "config.toml"), servingConfig)
	path := filepath.Join(home, "sessions", "rollout-day-"+testCodexID+".jsonl")
	move := func(provider string) string {
		return fmt.Sprintf(`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"model_provider_id":%q}}}`, provider) + "\n"
	}
	writeEvidenceFile(t, path, fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"model_provider":"cliproxyapi"}}`, testCodexID)+"\n"+
		turnContext("", "repo")+quotaRecord("null")+quotaRecord(testCodexLimits)+move("openai")+quotaRecord(testCodexLimits)+move("ollama")+quotaRecord("null"))
	c, _, evs := readSequence(t, path, quotaCursor{})
	want := []any{semconv.TermaServingBackendPrivate, semconv.TermaServingBackendPrivate, nil, semconv.TermaServingBackendPrivate}
	if len(evs) != len(want) {
		t.Fatalf("got %d records, want %d: %+v", len(evs), len(want), evs)
	}
	for i, ev := range evs {
		if ev.Attrs[semconv.TermaServingBackendKey] != want[i] {
			t.Errorf("record %d (%s): backend %v, want %v", i, ev.Status, ev.Attrs[semconv.TermaServingBackendKey], want[i])
		}
	}
	if c.Provider != "ollama" {
		t.Fatalf("cursor provider %q, want the one the thread moved to", c.Provider)
	}

	// A later read keeps the provider the cursor holds, not the session's first.
	appendRollout(t, path, quotaRecord(testCodexLimits))
	if _, _, evs = readSequence(t, path, c); len(evs) != 1 || evs[0].Attrs[semconv.TermaServingBackendKey] != semconv.TermaServingBackendPrivate {
		t.Fatalf("later read: %+v", evs)
	}
}

// A session through a proxy says so from its first stop, before any quota: the platform takes
// the backend from the latest quota record, the not_ready one included.
func TestCodexNotReadyQuotaCarriesTheServingBackend(t *testing.T) {
	env := fundingEnv(t)
	writeEvidenceFile(t, filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), servingConfig)
	path := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "rollout-day-funding-session.jsonl")
	hookruntest.WriteFile(t, filepath.Dir(path), filepath.Base(path), `{"type":"session_meta","payload":{"id":"funding-session","model_provider":"cliproxyapi"}}`+"\n"+turnContext("turn-a", env.Cwd))
	r, err := env.Repo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	captureCodexFunding(context.Background(), env, r, &codexHookInput{SessionID: "funding-session", TranscriptPath: path, Cwd: env.Cwd})
	evs := hookruntest.Spooled(t, env.Spool)
	if len(evs) != 1 || evs[0].Name != semconv.TermaSessionQuotaEvent || evs[0].Attrs[semconv.TermaEvidenceStatusKey] != "not_ready" ||
		evs[0].Attrs[semconv.TermaServingBackendKey] != semconv.TermaServingBackendPrivate {
		t.Fatalf("not_ready quota: %+v", evs)
	}
}
