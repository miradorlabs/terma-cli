package shim

import (
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// AgentOmp is omp's binary name.
const AgentOmp = "omp"

// ompRouter points omp at the local relay. omp's exporter is configured only through
// OTEL_* environment variables, read before any hook or extension loads (omp 18.3:
// initTelemetryExport runs ahead of createAgentSession), so its extension cannot set
// them in time; the launcher can. It exports only in a repository with a binding, on a
// machine where the relay is set up: elsewhere omp exports nothing at all, and what it
// exports the relay still forwards only for sessions a hook claimed.
type ompRouter struct{}

func (ompRouter) name() string { return AgentOmp }

func (ompRouter) workingDir(cwd string, _ []string) string { return cwd }

func (ompRouter) routeArgs(Record, []string) []string { return nil }

// routeEnv is the relay's endpoint and token, in OTLP's own variables.
func (ompRouter) routeEnv() map[string]string {
	token := claim.Token()
	if token == "" {
		return nil
	}
	return map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://" + claim.Addr(),
		"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
		"OTEL_EXPORTER_OTLP_HEADERS":  "Authorization=Bearer " + token,
	}
}
