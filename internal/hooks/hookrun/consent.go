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

// ConsentUnder is the consent a hook's content travels under in the working copy r, whose
// admitting policy decides global mode: the selected team's policy may not be r's.
func (e Env) ConsentUnder(r *Repo) Consent {
	c := Consent{Agents: e.Agents, Global: e.Policy.Global()}
	if r != nil {
		c.Global = r.Policy.Global()
	}
	return c
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
