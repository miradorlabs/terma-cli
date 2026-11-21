package omp

import (
	"os"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// ompConflicts reports shell-exported OTEL_* variables, which omp reads ahead of its .env:
// an endpoint there takes the export somewhere else.
func ompConflicts(e harness.Exporter) []harness.Conflict {
	var out []harness.Conflict

	endpoints := []string{harness.EnvOTLPEndpoint}
	for _, s := range harness.AllSignals {
		endpoints = append(endpoints, signalKey(s, "ENDPOINT"))
	}
	for _, key := range endpoints {
		value := strings.TrimRight(os.Getenv(key), "/")
		if value == "" || value == e.Endpoint || e.Endpoint != "" && strings.HasPrefix(value, e.Endpoint+"/v1/") {
			continue
		}
		out = append(out, harness.Conflict{
			Key:        key,
			Value:      value,
			Reason:     "exported in your shell, where omp reads it ahead of its .env — the export goes there, not to Terma",
			Credential: true,
			Scope:      harness.ScopeEnvironment,
			Clearable:  false,
		})
	}

	// omp supports http/protobuf only; any other protocol silently disables the export.
	if value := strings.ToLower(strings.TrimSpace(os.Getenv(harness.EnvOTLPProtocol))); value != "" && value != harness.ProtocolHTTPProtobuf {
		out = append(out, harness.Conflict{
			Key:       harness.EnvOTLPProtocol,
			Value:     value,
			Reason:    "omp's exporter supports http/protobuf only; a " + value + " export would disable every signal",
			Scope:     harness.ScopeEnvironment,
			Clearable: false,
		})
	}

	if value := os.Getenv(ompCaptureContentEnv); value != "" && !capturesContent(value) {
		out = append(out, harness.Conflict{
			Key:       ompCaptureContentEnv,
			Value:     value,
			Reason:    "exported in your shell, where it turns content capture off; the team's policy decides what leaves, so terma asks for all of it",
			Scope:     harness.ScopeEnvironment,
			Clearable: false,
			Advisory:  true,
		})
	}
	return out
}

// capturesContent reports whether a capture value keeps full content on, as omp reads it.
func capturesContent(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", ompCaptureContentFull:
		return true
	}
	return false
}
