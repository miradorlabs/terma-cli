package e2e

import "testing"

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
