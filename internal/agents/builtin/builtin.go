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
	"github.com/miradorlabs/terma-cli/internal/agents/grok"
	"github.com/miradorlabs/terma-cli/internal/agents/hermes"
	"github.com/miradorlabs/terma-cli/internal/agents/omp"
	"github.com/miradorlabs/terma-cli/internal/agents/opencode"
	"github.com/miradorlabs/terma-cli/internal/agents/pi"
)

// Agents is every agent terma knows in install order, and the supported ones; those that
// keep records do so under terma's config directory configDir.
func Agents(configDir string) *agents.Registry { return AgentsOn(configDir, runtime.GOOS) }

// AgentsOn is Agents on the operating system goos. Antigravity, Grok Build, OpenCode and Pi
// are Coming Soon on Windows: agy runs a hook there with cmd, and its hooks are checked only
// with the POSIX command; grok's hooks and export are verified on Linux alone; nothing of
// OpenCode's plugin or Pi's extension was captured on Windows, and the tests that run them
// skip there.
func AgentsOn(configDir, goos string) *agents.Registry {
	claudeCode, codexCLI := claude.Agent{ConfigDir: configDir}, codex.Agent{ConfigDir: configDir}
	githubCopilot, ompAgent, agy := copilot.Agent{ConfigDir: configDir}, omp.Agent{ConfigDir: configDir}, antigravity.Agent{ConfigDir: configDir}
	grokBuild, openCode, piAgent := grok.Agent{ConfigDir: configDir}, opencode.Agent{ConfigDir: configDir}, pi.Agent{}
	supported := []agents.Agent{claudeCode, codexCLI, githubCopilot, ompAgent}
	if goos != "windows" {
		supported = append(supported, agy, grokBuild, openCode, piAgent)
	}
	return agents.New(
		claudeCode,
		cursor.Agent{},
		codexCLI,
		githubCopilot,
		openCode,
		ompAgent,
		piAgent,
		hermes.Agent{},
		gemini.Agent{},
		dsh.Agent{},
		agy,
		grokBuild,
	).Support(supported...)
}
