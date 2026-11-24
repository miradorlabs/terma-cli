package hookrun

import (
	"github.com/miradorlabs/terma-cli/internal/config"
)

// Consent is what spooled conversation content (a reply, a thread's name) travels under,
// gathered fresh by whoever asks: a hook, or a delivery that predates a policy change.
// Neither runs once teardown has retired the relay's token (dispatch.Run, flushSpool).
type Consent struct {
	// Agents are the agents the developer chose at setup.
	Agents []string
	Global bool
}

// Consent is the consent this hook runs under for what it records in r: global when the
// policy r's records leave under (PolicyFor) collects every session, as delivery asks of
// the team's policy when they leave.
func (e Env) Consent(r *Repo) Consent {
	return Consent{Agents: e.Agents, Global: e.PolicyFor(r).Global()}
}

// ConsentFor is the consent the developer's setup gives now, under the config directory
// configDir; only its agents are read, so no state directory is needed.
func ConsentFor(configDir string, global bool) Consent {
	c := Consent{Global: global}
	if cfg, err := config.Load(configDir, "", config.Overrides{}); err == nil {
		c.Agents = cfg.Harnesses
	}
	return c
}
