package e2e

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Independent threads can land on opposite sides of Codex's 1% persistence sample.
// Regular metrics, logs and spans remain part of the equivalence contract.
func TestWorkloadComparisonAllowsIndependentPersistenceSamples(t *testing.T) {
	baseline := map[string]int{
		"metric codex.token_usage": 1,
		"log codex.user_prompt":    1,
		"span session_loop":        1,
	}
	sampled := map[string]int{
		"metric codex.token_usage":                           1,
		"log codex.user_prompt":                              1,
		"span session_loop":                                  1,
		"metric codex.rollout.persistence.append":            1,
		"metric codex.rollout.persistence.item_bytes":        1,
		"metric codex.rollout.persistence.turn_bytes":        1,
		"metric codex.rollout.persistence.measurement_error": 1,
	}
	compareShapes(t, sampled, baseline, false)
	compareShapes(t, baseline, sampled, false)
}

// A call offering no tools, such as dsh's session title, gets a reply and leaves the
// workload's tool step to the agent's own call, whichever arrives first.
func TestWorkloadProviderStepsOnlyCallsOfferingTools(t *testing.T) {
	var calls atomic.Int32
	provider := httptest.NewServer(claudeWorkloadProvider(&calls, []claudeStep{{bash("printf ok")}}, false))
	defer provider.Close()
	answer := func(body string) string {
		t.Helper()
		res, err := http.Post(provider.URL+"/v1/messages", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		return string(data)
	}
	if title := answer(`{"messages":[]}`); strings.Contains(title, "tool_use") {
		t.Errorf("a call offering no tools took the tool step:\n%s", title)
	}
	if agent := answer(`{"messages":[],"tools":[{"name":"Bash"}]}`); !strings.Contains(agent, `"stop_reason":"tool_use"`) {
		t.Errorf("the agent's call did not get the tool step:\n%s", agent)
	}
}
