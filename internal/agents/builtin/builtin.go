// Package builtin registers the coding agents this build of terma knows and supports.
package builtin

import (
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/antigravity"
	"github.com/miradorlabs/terma-cli/internal/agents/claude"
	"github.com/miradorlabs/terma-cli/internal/agents/codex"
	"github.com/miradorlabs/terma-cli/internal/agents/cursor"
	"github.com/miradorlabs/terma-cli/internal/agents/dsh"
	"github.com/miradorlabs/terma-cli/internal/agents/gemini"
	"github.com/miradorlabs/terma-cli/internal/agents/hermes"
	"github.com/miradorlabs/terma-cli/internal/agents/omp"
	"github.com/miradorlabs/terma-cli/internal/agents/opencode"
	"github.com/miradorlabs/terma-cli/internal/agents/pi"
)

// Agents is every agent terma knows in install order, the supported ones, and the ones
// only announced; those that keep records do so under terma's config directory configDir.
func Agents(configDir string) *agents.Registry {
	claudeCode, codexCLI := claude.Agent{ConfigDir: configDir}, codex.Agent{ConfigDir: configDir}
	return agents.New(
		claudeCode,
		cursor.Agent{},
		codexCLI,
		opencode.Agent{ConfigDir: configDir},
		omp.Agent{ConfigDir: configDir},
		pi.Agent{},
		hermes.Agent{},
		gemini.Agent{},
		dsh.Agent{},
		antigravity.Agent{},
	).Support(claudeCode, codexCLI).Upcoming("GitHub Copilot")
}
