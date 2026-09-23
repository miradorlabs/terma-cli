package cmd

import (
	"testing"

	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

// routedPerRepo is load-bearing: status and doctor must agree, via this function, on
// whether a repository's sessions actually reach its project. It reports routed only when
// the agent is routed for the project AND its per-project key has been minted.
func TestRoutedPerRepo(t *testing.T) {
	const project = "6796a71f-7949-40f1-bde8-b87a74071686"
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())

	if err := shim.SaveRecord(shim.Record{ProjectID: project, Harnesses: []string{shim.AgentClaude}}); err != nil {
		t.Fatal(err)
	}
	if err := keystore.SetFor(shim.AgentClaude, project, "ter_srv_0123456789abcdef"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, project string
		want          bool
		why           string
	}{
		{shim.AgentClaude, project, true, "routed for the project with a minted key"},
		{shim.AgentClaude, "", false, "no project id"},
		{shim.AgentCodex, project, false, "not in the record's harnesses"},
		{"cursor", project, false, "not a routable agent"},
	} {
		if got := routedPerRepo(tc.name, tc.project); got != tc.want {
			t.Errorf("routedPerRepo(%q, %q) = %v, want %v (%s)", tc.name, tc.project, got, tc.want, tc.why)
		}
	}

	// Configured but keyless: the record routes Codex too now, but no Codex key was
	// minted, so its sessions cannot deliver — routed must stay false. This is exactly
	// the "configured but not connected" state status and doctor must not call connected.
	if err := shim.SaveRecord(shim.Record{ProjectID: project, Harnesses: []string{shim.AgentClaude, shim.AgentCodex}}); err != nil {
		t.Fatal(err)
	}
	if routedPerRepo(shim.AgentCodex, project) {
		t.Error("codex is in the record but has no key; must not report routed")
	}

	// OpenCode routes itself and is always considered live once configured.
	if !perRepoLive("opencode") {
		t.Error("perRepoLive(opencode) = false, want true")
	}
}
