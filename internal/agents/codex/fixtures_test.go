package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

const testCodexID = "019947ab-1234-7000-8000-123456789abc"

func rolloutFixture(id string, at time.Time, limits string) string {
	return fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cli_version\":\"0.154.0\",\"cwd\":\"private-repo\"}}\n{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"total_tokens\":999},\"rate_limits\":%s}}\n", id, at.Format(time.RFC3339Nano), limits)
}

const testCodexLimits = `{"plan_type":"team","primary":{"used_percent":0,"window_minutes":300,"resets_at":1800000000},"secondary":null,"credits":{"has_credits":false,"unlimited":false,"balance":"12.345678901234567890"},"rate_limit_reached_type":"workspace_owner_credits_depleted","spend_control_reached":true,"secret":"never-export"}`
const termaEndpoint = "https://otel.terma.ai"

func termaExporter() harness.Exporter {
	return harness.Exporter{Endpoint: termaEndpoint, Signals: harness.AllSignals}
}

func fullExporter() harness.Exporter {
	return harness.Exporter{
		Endpoint:  "https://otel.terma.ai",
		APIKey:    "ter_srv_0123456789abcdef",
		ProjectID: "proj_123",
		Signals:   harness.AllSignals,
		ResourceAttributes: map[string]string{
			harness.AttrServiceName: "claude-code",
			harness.AttrEnduserID:   "dev@example.com",
			harness.AttrProjectID:   "proj_123",
		},
	}
}

func writeEvidenceFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func quotaAttrs(e harness.FundingEvidence) map[string]any {
	out := map[string]any{}
	for k, v := range e.Attrs {
		switch k {
		case "source_offset", "source_stream", "observation_id", "turn_id":
		default:
			out[k] = v
		}
	}
	return out
}
