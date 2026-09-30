package routing

import (
	"os"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

func TestPolicyCacheIsolatesTeamsAndScopes(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	a := config.DefaultPolicy()
	a.TeamID, a.OrganizationID, a.AuthURL = "team-a", "org", "https://dev.example"
	a.Revision, a.FetchedAt, a.IncludePrompts = 9, time.Now(), false
	if err := SavePolicy(a); err != nil {
		t.Fatal(err)
	}
	b := a
	b.TeamID, b.Revision, b.IncludePrompts = "team-b", 1, true
	if err := SavePolicy(b); err != nil {
		t.Fatal(err)
	}
	if EffectivePolicy(b, "team-a").IncludePrompts || !EffectivePolicy(a, "team-b").IncludePrompts {
		t.Fatal("borrowed another team's capture policy")
	}
	if EffectivePolicy(b, "unknown").AllowsSignal("logs") {
		t.Fatal("unknown team borrowed selected team's grant")
	}
	other := b
	other.OrganizationID = "other"
	if EffectivePolicy(other, "team-b").AllowsSignal("logs") {
		t.Fatal("policy crossed organizations")
	}
	other = b
	other.AuthURL = "https://prod.example"
	if EffectivePolicy(other, "team-b").AllowsSignal("logs") {
		t.Fatal("policy crossed environments")
	}
	a.Revision = 8
	if err := SavePolicy(a); err == nil {
		t.Fatal("accepted older revision")
	}
	path, err := policyPath("team-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if EffectivePolicy(b, "team-a").AllowsSignal("logs") {
		t.Fatal("corrupt cache defaulted to capture")
	}
}
