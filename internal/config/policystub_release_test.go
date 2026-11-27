//go:build release

package config

import (
	"testing"
	"time"
)

// A release build ignores the offline stub: it is no policy, and stands in for no team.
// internal/boundary runs this with the release tag.
func TestReleaseBuildIgnoresThePolicyStub(t *testing.T) {
	t.Setenv("TERMA_POLICY_STUB", `{"mode":"global","include_prompts":true,"include_tool_content":true}`)
	if got := PolicyStub(); got != "" {
		t.Fatalf("PolicyStub() = %q in a release build", got)
	}
	if (Policy{FetchedAt: time.Now()}).Validated() {
		t.Fatal("the stub stood in for a team in a release build")
	}
}
