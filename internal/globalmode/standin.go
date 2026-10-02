package globalmode

import (
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/project"
)

// standsIn reports whether a machine-wide hook outside global mode does the work of a
// repository's committed hooks its agent cannot see: the agent reads them only from where
// it started, a subdirectory of a bound repository whose root carries them. Started at the
// root, or where a settings file of terma's own sits, the committed hooks run and this one
// steps aside, so no hook fires twice.
func (m Machine) standsIn(tool, cwd string) bool {
	if cwd == "" {
		return false
	}
	a, ok := m.Agents.ForTool(tool)
	if !ok {
		return false
	}
	scoped, ok := a.(agents.LaunchScoped)
	if !ok {
		return false
	}
	launch := filepath.Clean(scoped.LaunchDir(cwd))
	root, gitDir, ok := gitx.LocateFS(launch)
	if !ok || filepath.Clean(root) == launch {
		return false
	}
	if f, _, err := project.Resolve(root, gitDir); err != nil || f == nil {
		return false
	}
	return agents.Wired(root, a) && !agents.Wired(launch, a)
}
