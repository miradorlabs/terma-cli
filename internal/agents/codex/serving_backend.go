package codex

import (
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// codexOpenAIProvider is the id of Codex's built-in OpenAI provider, which config.toml's
// model_providers cannot redefine: only openai_base_url moves it.
const codexOpenAIProvider = "openai"

// codexRoutes is what config.toml says about where each provider sends its calls. It is
// today's file, not the session's: a base_url passed with -c, a profile file or a managed
// layer is not seen, and a provider it does not define maps to nothing.
type codexRoutes struct {
	// readable is false when config.toml exists but cannot be read or parsed.
	readable      bool
	openAIBaseURL string
	baseURLs      map[string]string
}

// readCodexRoutes reads the model providers' base URLs from $CODEX_HOME/config.toml.
func readCodexRoutes() codexRoutes {
	path, err := (exporter{}).ConfigPath()
	if err != nil {
		return codexRoutes{}
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return codexRoutes{readable: true}
	}
	if err != nil {
		return codexRoutes{}
	}
	var doc struct {
		OpenAIBaseURL  string `toml:"openai_base_url"`
		ModelProviders map[string]struct {
			BaseURL string `toml:"base_url"`
		} `toml:"model_providers"`
	}
	if unmarshalTOMLLenient(data, &doc) != nil {
		return codexRoutes{}
	}
	routes := codexRoutes{readable: true, openAIBaseURL: strings.TrimSpace(doc.OpenAIBaseURL), baseURLs: map[string]string{}}
	for id, p := range doc.ModelProviders {
		routes.baseURLs[id] = strings.TrimSpace(p.BaseURL)
	}
	return routes
}

// backend maps the session's model provider id to terma.serving.backend's value, "" when the
// route is not one it can name. The id is the developer's own name for a custom provider, so
// only where it sends calls decides.
func (c codexRoutes) backend(provider string) string {
	if provider == "" || !c.readable {
		return ""
	}
	if provider == codexOpenAIProvider {
		base := cmp.Or(c.openAIBaseURL, strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")))
		if base == "" {
			return semconv.TermaServingBackendOpenai
		}
		return codexBaseURLBackend(base)
	}
	return codexBaseURLBackend(c.baseURLs[provider])
}

// codexBaseURLBackend recognises OpenAI's and Azure OpenAI's hosts; any other host (a gateway,
// a proxy, a local server) is "", since what stands behind it is unknown.
func codexBaseURLBackend(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	switch {
	case host == "api.openai.com":
		return semconv.TermaServingBackendOpenai
	case strings.HasSuffix(host, ".openai.azure.com"), strings.HasSuffix(host, ".services.ai.azure.com"),
		strings.HasSuffix(host, ".cognitiveservices.azure.com"):
		return semconv.TermaServingBackendAzureOpenai
	}
	return ""
}

// codexSessionProvider is the model provider a rollout's session_meta line names.
func codexSessionProvider(line []byte) string {
	var meta struct {
		Payload struct {
			ModelProvider string `json:"model_provider"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &meta) != nil {
		return ""
	}
	return meta.Payload.ModelProvider
}
