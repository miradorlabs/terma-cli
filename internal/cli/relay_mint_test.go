package cli

import (
	"errors"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/keystore"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

const mintedKey = "ter_srv_minted0123456789abcdefghijklmnopqrstuv"

// A claimed project without a key here is minted one; with a key, the organization's
// policy decides content.
func TestRelayResolverMintsAndCapsContent(t *testing.T) {
	useConfigDir(t, t.TempDir())
	cfg := &config.Config{Dir: testApp.dir, OTLPURL: "https://otel.example", Policy: config.DefaultPolicy(), Harnesses: []string{"codex"}}
	var asked []string
	resolve := testApp.relayDeps().Resolver(cfg, func(p string) { asked = append(asked, p) })
	c := claim.Claim{ProjectID: "p1", Tool: "codex"}
	if _, err := resolve(c); !errors.Is(err, relay.ErrNoKey) || len(asked) != 1 || asked[0] != "p1" {
		t.Fatalf("keyless: err %v, asked %v", err, asked)
	}
	if err := keystore.Set(testApp.dir, "p1", mintedKey, keystore.HostsOf(cfg)); err != nil {
		t.Fatal(err)
	}
	pol, err := resolve(c)
	if err != nil || !pol.IncludePrompts || !pol.IncludeToolContent || pol.Key != mintedKey {
		t.Fatalf("keyed: %+v, %v (the policy's defaults apply)", pol, err)
	}
	cfg.Policy.IncludeToolContent = false
	if pol, _ := resolve(c); pol.IncludeToolContent {
		t.Fatalf("the organization's tool-content-off was not applied: %+v", pol)
	}
}
