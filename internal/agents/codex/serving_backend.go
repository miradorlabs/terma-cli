package codex

import (
	"encoding/json"
	"os"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents/internal/servinghost"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// codexOpenAIProvider is Codex's built-in OpenAI provider, which config.toml's model_providers
// cannot redefine: only the top-level openai_base_url moves it (Codex reads no
// OPENAI_BASE_URL).
const codexOpenAIProvider = "openai"

// codexLocalProviders are Codex's built-in providers that run a model on the developer's
// machine.
var codexLocalProviders = []string{"ollama", "lmstudio", "oss"}

// codexCloudPrefix starts the ids of Codex's built-in Amazon Bedrock providers, a named cloud.
const codexCloudPrefix = "amazon-bedrock"

// codexProviders is what config.toml says about where model providers send their calls: today's
// $CODEX_HOME/config.toml, not the session's. A base_url passed with -c or set in another
// config layer is not seen.
type codexProviders struct {
	openAIBaseURL string
	baseURLs      map[string]string
}

// readCodexProviders reads the providers' base URLs from config.toml; none when it is missing
// or unreadable.
func readCodexProviders() codexProviders {
	path, err := (exporter{}).ConfigPath()
	if err != nil {
		return codexProviders{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return codexProviders{}
	}
	var doc struct {
		OpenAIBaseURL  string `toml:"openai_base_url"`
		ModelProviders map[string]struct {
			BaseURL string `toml:"base_url"`
		} `toml:"model_providers"`
	}
	if unmarshalTOMLLenient(data, &doc) != nil {
		return codexProviders{}
	}
	p := codexProviders{openAIBaseURL: doc.OpenAIBaseURL, baseURLs: map[string]string{}}
	for id, mp := range doc.ModelProviders {
		p.baseURLs[id] = mp.BaseURL
	}
	return p
}

// codexServingBackend is terma.serving.backend for a session on provider: private for a
// built-in local provider, or a provider whose base URL names a host that is not OpenAI's or a
// named cloud's (a gateway, a proxy, a local server); "" for OpenAI, Azure and Bedrock, an
// unknown provider, or one whose base URL config.toml does not name.
func codexServingBackend(provider string, p codexProviders) string {
	switch {
	case provider == "" || strings.HasPrefix(provider, codexCloudPrefix):
		return ""
	case slices.Contains(codexLocalProviders, provider):
		return semconv.TermaServingBackendPrivate
	case provider == codexOpenAIProvider:
		return servinghost.Private(p.openAIBaseURL)
	}
	return servinghost.Private(p.baseURLs[provider])
}

// codexSessionProvider is the model provider a rollout's session_meta line names, "" when the
// line cannot be read (a first line longer than the head read).
func codexSessionProvider(line []byte) string {
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			ModelProvider string `json:"model_provider"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &meta) != nil || meta.Type != "session_meta" {
		return ""
	}
	return meta.Payload.ModelProvider
}
