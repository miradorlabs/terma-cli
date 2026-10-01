package doctor

import (
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
)

const testKey = "ter_srv_minted0123456789abcdefghijklmnopqrstuv"

// Either key serves a surface: the agent's own, or the project's.
func TestASurfaceIsKeyedByItsAgentsKeyOrTheProjects(t *testing.T) {
	for _, store := range []func() error{
		func() error { return keystore.SetFor("agent-a", "p1", testKey, keystore.Hosts{}) },
		func() error { return keystore.Set("p1", testKey, keystore.Hosts{}) },
	} {
		t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
		in := SurfaceInput(t.TempDir(), "p1")
		if in.Keyed("agent-a") {
			t.Fatal("keyed with no key stored")
		}
		if err := store(); err != nil {
			t.Fatal(err)
		}
		if !in.Keyed("agent-a") {
			t.Fatal("a stored key did not serve")
		}
	}
}
