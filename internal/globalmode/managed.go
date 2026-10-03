package globalmode

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// WriteManaged writes the managed configuration an administrator deploys into dir. It needs
// no sign-in; terma is the path the hooks call it by on the deployed machines, with $HOME
// expanded per user.
func WriteManaged(reg *agents.Registry, dir, terma string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	cmd := hookmgr.ManagedHookCommand(terma)
	managed := reg.With[agents.ManagedHooks]()
	var names, deploy []string
	files := map[string][]byte{}
	for _, a := range managed {
		name, data, err := a.ManagedConfig(cmd)
		if err != nil {
			return nil, err
		}
		files[name] = data
		names = append(names, a.DisplayName())
		deploy = append(deploy, "- `"+name+"` → "+a.ManagedDeploy())
	}
	files["README.md"] = []byte(`# terma: managed hooks

Deploy these so every ` + strings.Join(names, " and ") + ` session on a machine runs terma's hooks,
with no trust step for anyone; the team's folder list still decides what is recorded.
Each developer still runs ` + "`terma setup`" + ` once: it points the agents' exporters at the
machine's relay, whose token is the machine's own.

` + strings.Join(deploy, "\n") + `

The hooks run terma as ` + "`" + terma + "`" + `; terma must be installed there for every user.
Where these are deployed, ` + "`terma setup`" + ` writes no per-user hooks of its own.
`)
	var out []string
	for name, data := range files {
		p := filepath.Join(dir, name)
		if err := config.WriteFileAtomic(p, data, 0o644); err != nil {
			return out, err
		}
		out = append(out, p)
	}
	slices.Sort(out)
	return out, nil
}
