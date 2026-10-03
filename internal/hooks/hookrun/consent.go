package hookrun

import (
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// Consent is what spooled conversation content (a reply, a thread's name) travels under,
// gathered fresh by whoever asks: a hook, or a delivery that predates a policy change.
type Consent struct {
	// Agents are the agents the developer chose at setup.
	Agents []string
	// Relay is the local relay set up on this machine.
	Relay  bool
	Global bool
}

// Consent is the consent this hook runs under.
func (e Env) Consent() Consent {
	return Consent{Agents: e.Agents, Relay: claim.Enabled(), Global: e.Policy.Global()}
}

// ConsentFor is the consent the developer's setup gives now.
func ConsentFor(global bool) Consent {
	c := Consent{Relay: claim.Enabled(), Global: global}
	if cfg, err := config.Load(config.Overrides{}); err == nil {
		c.Agents = cfg.Harnesses
	}
	return c
}
