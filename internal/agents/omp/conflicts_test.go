package omp

import (
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// A shell endpoint on the relay's host carries the relay's token in its path: the conflict
// names it with the token masked, never the token; another host's stays as it is.
func TestOmpConflictsNeverHoldTheRelayToken(t *testing.T) {
	relay := "http://127.0.0.1:43180/tok-secret"
	t.Setenv(signalKey(harness.SignalTraces, "ENDPOINT"), relay)
	t.Setenv(signalKey(harness.SignalLogs, "ENDPOINT"), relay+"/v1/traces")
	t.Setenv(signalKey(harness.SignalMetrics, "ENDPOINT"), "https://collector.example/v1/metrics")
	values := map[string]string{}
	for _, c := range ompConflicts(harness.Exporter{Endpoint: relay}) {
		if strings.Contains(c.Value, "tok-secret") {
			t.Errorf("conflict %s holds the token: %q", c.Key, c.Value)
		}
		values[c.Key] = c.Value
	}
	for key, want := range map[string]string{
		signalKey(harness.SignalTraces, "ENDPOINT"):  "http://127.0.0.1:43180/***",
		signalKey(harness.SignalLogs, "ENDPOINT"):    "http://127.0.0.1:43180/***/v1/traces",
		signalKey(harness.SignalMetrics, "ENDPOINT"): "https://collector.example/v1/metrics",
	} {
		if values[key] != want {
			t.Errorf("%s = %q, want %q", key, values[key], want)
		}
	}
}
