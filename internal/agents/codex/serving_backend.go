package codex

import (
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents/internal/servinghost"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// codexOpenAIProvider is the id of Codex's built-in OpenAI provider, which config.toml's
// model_providers cannot redefine: only openai_base_url moves it.
const codexOpenAIProvider = "openai"

// codexLocalProviders are Codex's built-in providers that run on the developer's machine.
var codexLocalProviders = []string{"ollama", "lmstudio", "oss"}

// codexCloudProviders are Codex's built-in cloud providers and their backends; config.toml can
// move one only by setting its base_url.
var codexCloudProviders = map[string]string{
	"amazon-bedrock":         semconv.TermaServingBackendAwsBedrock,
	"amazon-bedrock-runtime": semconv.TermaServingBackendAwsBedrock,
	"gcp":                    semconv.TermaServingBackendGoogleVertex,
}

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

// backend maps the session's model provider id to terma.serving.backend's value, "" when
// nothing says where its calls go. The id is the developer's own name for a custom provider,
// so only where it sends calls decides.
func (c codexRoutes) backend(provider string) string {
	if slices.Contains(codexLocalProviders, provider) {
		return semconv.TermaServingBackendPrivate
	}
	if provider == "" || !c.readable {
		return ""
	}
	if provider == codexOpenAIProvider {
		// Codex reads only openai_base_url for its built-in provider, never OPENAI_BASE_URL.
		if c.openAIBaseURL == "" {
			return semconv.TermaServingBackendOpenai
		}
		return codexBaseURLBackend(c.openAIBaseURL)
	}
	if cloud, ok := codexCloudProviders[provider]; ok {
		base := c.baseURLs[provider]
		if base == "" {
			return cloud
		}
		// Another provider's host is as foreign to this one as a gateway's.
		if b := codexBaseURLBackend(base); b == "" || b == cloud {
			return b
		}
		return semconv.TermaServingBackendPrivate
	}
	return codexBaseURLBackend(c.baseURLs[provider])
}

// codexHostBackends are the backends serving Codex's providers from each kind of host.
var codexHostBackends = map[servinghost.Kind]string{
	servinghost.OpenAI:  semconv.TermaServingBackendOpenai,
	servinghost.Azure:   semconv.TermaServingBackendAzureOpenai,
	servinghost.Bedrock: semconv.TermaServingBackendAwsBedrock,
	servinghost.Vertex:  semconv.TermaServingBackendGoogleVertex,
}

// codexBaseURLBackend is the backend serving raw's host; any other host (a gateway, Azure
// API Management, a proxy, a local server) is private, and a URL with no host, or none at
// all, is "".
func codexBaseURLBackend(raw string) string {
	kind, ok := servinghost.Of(raw)
	if !ok {
		return ""
	}
	return cmp.Or(codexHostBackends[kind], semconv.TermaServingBackendPrivate)
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
