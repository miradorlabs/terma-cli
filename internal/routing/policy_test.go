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
	if !EffectivePolicy(b, "unknown").CollectsNothing {
		t.Fatal("unknown team borrowed selected team's grant")
	}
	other := b
	other.OrganizationID = "other"
	if !EffectivePolicy(other, "team-b").CollectsNothing {
		t.Fatal("policy crossed organizations")
	}
	other = b
	other.AuthURL = "https://prod.example"
	if !EffectivePolicy(other, "team-b").CollectsNothing {
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
	if !EffectivePolicy(b, "team-a").CollectsNothing {
		t.Fatal("corrupt cache defaulted to capture")
	}
}

// An expired policy grants nothing, whether it is the team's cache or the fallback.
func TestExpiredPolicyGrantsNothing(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	old := config.DefaultPolicy()
	old.TeamID, old.OrganizationID, old.AuthURL = "team-a", "org", "https://dev.example"
	old.Revision, old.FetchedAt = 1, time.Now().Add(-config.MaxPolicyAge-time.Hour)
	if err := SavePolicy(old); err != nil {
		t.Fatal(err)
	}
	if got := EffectivePolicy(old, "team-a"); !got.CollectsNothing || got.IncludePrompts {
		t.Fatalf("an expired cache granted %+v", got)
	}
	fallback := old
	fallback.TeamID = "team-b"
	if got := EffectivePolicy(fallback, "team-b"); !got.CollectsNothing || got.IncludePrompts {
		t.Fatalf("an expired fallback granted %+v", got)
	}
	fresh := old
	fresh.Revision, fresh.FetchedAt = 2, time.Now()
	if err := SavePolicy(fresh); err != nil {
		t.Fatal(err)
	}
	if EffectivePolicy(fresh, "team-a").CollectsNothing {
		t.Fatal("a refreshed policy still grants nothing")
	}
}
