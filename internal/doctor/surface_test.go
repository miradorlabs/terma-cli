package doctor

import "testing"

// Either key serves a surface: the agent's own, or the project's.
func TestASurfaceIsKeyedByItsAgentsKeyOrTheProjects(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	for _, stored := range []map[[2]string]string{
		{{"agent-a", "p1"}: "ter_srv_…"},
		{{"", "p1"}: "ter_srv_…"},
	} {
		keys := Keys(func(agent, project string) string { return stored[[2]string{agent, project}] })
		in := SurfaceInput(keys, t.TempDir(), "p1")
		if !in.Keyed("agent-a") {
			t.Fatalf("a stored key did not serve: %v", stored)
		}
		if other := SurfaceInput(keys, t.TempDir(), "p2"); other.Keyed("agent-a") {
			t.Fatal("another project's key served")
		}
	}
	if SurfaceInput(nil, t.TempDir(), "p1").Keyed("agent-a") {
		t.Fatal("keyed with no keystore at all")
	}
}
