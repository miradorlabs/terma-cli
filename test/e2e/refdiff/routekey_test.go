package refdiff

import (
	"strings"
	"testing"
)

// A telemetry route is known by its control, so one control's new outcome is one change.
func TestTelemetryRouteKey(t *testing.T) {
	route := func(outcome string) map[string]any {
		return map[string]any{"control": "settings.json telemetry.endpoint", "outcome": outcome}
	}
	var out []Change
	diffValue([]string{"network", "telemetry_routing"}, []any{route("redirected")}, []any{route("off")}, &out)
	if len(out) != 1 || out[0].Op != "changed" || strings.Join(out[0].Path, ".") != "network.telemetry_routing.settings.json telemetry.endpoint.outcome" {
		t.Errorf("changes = %+v", out)
	}
}
