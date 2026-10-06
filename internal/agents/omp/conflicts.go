package omp

import (
	_ "embed"
	"os"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// ConflictsWith reports shell exports that would defeat or redirect e.
func (c exporter) ConflictsWith(e harness.Exporter) ([]harness.Conflict, error) {
	return ompConflicts(e), nil
}

// ompConflicts reports shell-exported OTEL_* variables that outrank the extension, which
// never overrides what a developer exported on purpose.
func ompConflicts(e harness.Exporter) []harness.Conflict {
	var out []harness.Conflict

	if value := os.Getenv(harness.EnvOTLPEndpoint); value != "" && value != e.Endpoint {
		out = append(out, harness.Conflict{
			Key:        harness.EnvOTLPEndpoint,
			Value:      value,
			Reason:     "exported in your shell, where it takes effect regardless of what Terma's extension sets — the export goes there, not to Terma",
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

// capturesContent reports whether an exported capture value keeps content on, as terma
// always asks.
func capturesContent(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "summary":
		return true
	}
	return false
}
