package config

import "fmt"

// Environment names. Only production is public: `terma` never mentions the others
// in help output. They exist so Terma's own engineers can point the same binary at
// pre-production (TERMA_ENV=dev) or a local stack (TERMA_ENV=local) without a
// separate build, and so a profile can pin one.
const (
	EnvProd  = "prod"
	EnvDev   = "dev"
	EnvLocal = "local"
)

// Endpoints is one environment's set of hosts. Three separate remote hosts on
// purpose: an auth outage cannot take reads down with it, and a credential is bound
// to the auth host that minted it.
type Endpoints struct {
	// APIURL is the data plane (traces, logs, metrics). AuthURL is the credential
	// surface (CLI token exchange, whoami, projects, server keys). AppURL hosts the
	// browser page that approves a CLI login (/cli/auth).
	APIURL  string
	AuthURL string
	AppURL  string
	// OTLPURL is the telemetry ingest host. The CLI writes it into a harness's own
	// configuration (the harness exports there directly) and flushes its own event
	// spool to it.
	OTLPURL string
}

// Built-in environments. Override any host with TERMA_API_URL / TERMA_AUTH_URL /
// TERMA_APP_URL / TERMA_OTLP_URL, or store them on a profile with `terma config set`.
//
// Terma is a product on the shared Mirador backend, not a separate stack. In
// pre-production that shows through the hostnames: the app is Terma's own, while
// the auth, data, and ingest planes are the Mirador dev deployment, which serves
// Terma organizations and projects. Production keeps the terma.ai names because
// that is how the product will ship; those records do not exist yet, so a
// non-prod environment must be selected explicitly today.
var environments = map[string]Endpoints{
	EnvProd: {
		APIURL:  "https://api.terma.ai",
		AuthURL: "https://auth.terma.ai",
		AppURL:  "https://app.terma.ai",
		OTLPURL: "https://otel.terma.ai",
	},
	EnvDev: {
		APIURL:  "https://api-dev.mirador.org",
		AuthURL: "https://auth-dev.mirador.org",
		AppURL:  "https://dev.terma.ai",
		OTLPURL: "https://otel-dev.mirador.org",
	},
	EnvLocal: {
		// A terma-frontend dev server in front of the dev backend: the shape a
		// Terma engineer runs while working on the app itself. Only the app is
		// local — there is no local account service to mint a CLI credential —
		// and cleartext is allowed for loopback only.
		APIURL:  "https://api-dev.mirador.org",
		AuthURL: "https://auth-dev.mirador.org",
		AppURL:  "http://localhost:3000",
		OTLPURL: "https://otel-dev.mirador.org",
	},
}

// Production defaults, kept as named constants because the rest of the package and
// the tests refer to "the default" for the public environment.
const (
	DefaultAPIURL  = "https://api.terma.ai"
	DefaultAuthURL = "https://auth.terma.ai"
	DefaultAppURL  = "https://app.terma.ai"
	DefaultOTLPURL = "https://otel.terma.ai"
)

// EndpointsFor resolves a named environment. Unknown names are an error rather than a
// silent fall-through to production: a typo in TERMA_ENV must not quietly send a
// pre-production login to the real auth host.
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
