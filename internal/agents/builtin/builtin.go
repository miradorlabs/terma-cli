// Package builtin registers the coding agents this build of terma knows and supports.
package builtin

import (
	"runtime"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/antigravity"
	"github.com/miradorlabs/terma-cli/internal/agents/claude"
	"github.com/miradorlabs/terma-cli/internal/agents/codex"
	"github.com/miradorlabs/terma-cli/internal/agents/copilot"
	"github.com/miradorlabs/terma-cli/internal/agents/cursor"
	"github.com/miradorlabs/terma-cli/internal/agents/dsh"
	"github.com/miradorlabs/terma-cli/internal/agents/gemini"
	"github.com/miradorlabs/terma-cli/internal/agents/hermes"
	"github.com/miradorlabs/terma-cli/internal/agents/omp"
	"github.com/miradorlabs/terma-cli/internal/agents/opencode"
	"github.com/miradorlabs/terma-cli/internal/agents/pi"
)

// Agents is every agent terma knows in install order, and the supported ones; those that
// keep records do so under terma's config directory configDir.
func Agents(configDir string) *agents.Registry { return AgentsOn(configDir, runtime.GOOS) }

// AgentsOn is Agents on the operating system goos. Antigravity is Coming Soon on Windows:
// agy runs a hook there with cmd, and its hooks are checked only with the POSIX command.
func AgentsOn(configDir, goos string) *agents.Registry {
	claudeCode, codexCLI := claude.Agent{ConfigDir: configDir}, codex.Agent{ConfigDir: configDir}
	githubCopilot, ompAgent, agy := copilot.Agent{ConfigDir: configDir}, omp.Agent{ConfigDir: configDir}, antigravity.Agent{}
	supported := []agents.Agent{claudeCode, codexCLI, githubCopilot, ompAgent}
	if goos != "windows" {
		supported = append(supported, agy)
	}
	return agents.New(
		claudeCode,
		cursor.Agent{},
		codexCLI,
		githubCopilot,
		opencode.Agent{ConfigDir: configDir},
		ompAgent,
		pi.Agent{},
		hermes.Agent{},
		gemini.Agent{},
		dsh.Agent{},
		agy,
	).Support(supported...)
}
