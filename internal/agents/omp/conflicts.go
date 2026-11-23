package omp

import (
	"os"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// ompConflicts reports shell-exported OTEL_* variables, which omp reads key by key ahead of
// its .env: a signal's endpoint there, unless it is the relay's own URL for that signal, which
// omp sends to as given, takes that export somewhere else. The common endpoint and protocol do
// not, since omp reads each signal's own first and the .env sets every one.
func ompConflicts(e harness.Exporter) []harness.Conflict {
	var out []harness.Conflict

	for _, s := range harness.AllSignals {
		key := signalKey(s, "ENDPOINT")
		value := strings.TrimRight(os.Getenv(key), "/")
		if value == "" || e.Endpoint != "" && value == e.Endpoint+"/v1/"+string(s) {
			continue
		}
		out = append(out, harness.Conflict{
			Key:        key,
			Value:      value,
			Reason:     "exported in your shell, where OMP reads it ahead of its .env — the export goes there, not to Terma",
			Credential: true,
			Scope:      harness.ScopeEnvironment,
			Clearable:  false,
		})
	}

	if value := os.Getenv(envSDKDisabled); strings.EqualFold(strings.TrimSpace(value), "true") {
		out = append(out, harness.Conflict{
			Key:       envSDKDisabled,
			Value:     value,
			Reason:    "exported in your shell, where it switches OMP's whole export off",
			Scope:     harness.ScopeEnvironment,
			Clearable: false,
		})
	}
	// A signal switched off: only traces name omp's session, so logs or metrics off is advisory.
	for _, s := range harness.AllSignals {
		var off []harness.Conflict
		if value := strings.ToLower(strings.TrimSpace(os.Getenv(signalKey(s, "PROTOCOL")))); value != "" && value != harness.ProtocolHTTPProtobuf {
			off = append(off, harness.Conflict{Key: signalKey(s, "PROTOCOL"), Value: value,
				Reason: "OMP's exporter supports http/protobuf only; a " + value + " export would disable its " + string(s)})
		}
		if value := os.Getenv(signalExporterKey(s)); value != "" && !exportsOTLP(value) {
			off = append(off, harness.Conflict{Key: signalExporterKey(s), Value: value,
				Reason: "exported in your shell, where it switches OMP's " + string(s) + " export off"})
		}
		for _, c := range off {
			c.Scope, c.Advisory = harness.ScopeEnvironment, s != harness.SignalTraces
			out = append(out, c)
		}
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
