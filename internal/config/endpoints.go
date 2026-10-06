package config

import "fmt"

// Environment names. Only production is public; help text never mentions the others.
const (
	EnvProd  = "prod"
	EnvDev   = "dev"
	EnvLocal = "local"
)

// Endpoints is one environment's hosts; a credential is bound to the auth host that minted it.
type Endpoints struct {
	// APIURL is the data plane, AuthURL the credential surface, AppURL the login approval page.
	APIURL  string
	AuthURL string
	AppURL  string
	// OTLPURL is the ingest host agents and the spool export to.
	OTLPURL string
}

// environments are the built-in hosts; TERMA_*_URL or `terma config set` overrides any.
// Pre-production runs on the shared Mirador dev backend.
var environments = map[string]Endpoints{
	EnvProd: {
		APIURL:  "https://api.terma.ai",
		AuthURL: "https://auth.terma.ai",
		AppURL:  "https://app.terma.ai",
		OTLPURL: "https://otel.terma.ai",
	},
	EnvDev: {
		APIURL:  "https://api-dev.terma.ai",
		AuthURL: "https://auth-dev.terma.ai",
		AppURL:  "https://dev.terma.ai",
		OTLPURL: "https://otel-dev.terma.ai",
	},
	EnvLocal: {
		// A local app in front of the dev backend: there is no local account service.
		APIURL:  "https://api-dev.terma.ai",
		AuthURL: "https://auth-dev.terma.ai",
		AppURL:  "http://localhost:3000",
		OTLPURL: "https://otel-dev.terma.ai",
	},
}

// Production defaults.
const (
	DefaultAPIURL  = "https://api.terma.ai"
	DefaultAuthURL = "https://auth.terma.ai"
	DefaultAppURL  = "https://app.terma.ai"
	DefaultOTLPURL = "https://otel.terma.ai"
)

// EndpointsFor resolves a named environment; an unknown name is an error, so a typo
// never sends a pre-production login to production.
func EndpointsFor(env string) (Endpoints, error) {
	if env == "" {
		env = EnvProd
	}
	e, ok := environments[env]
	if !ok {
		return Endpoints{}, fmt.Errorf("unknown environment %q (want %s, %s or %s)", env, EnvProd, EnvDev, EnvLocal)
	}
	return e, nil
}
