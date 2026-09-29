package shim

import (
	"context"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/compat"
)

// Runtime telemetry overrides already require embedded mode. Make that choice
// explicit on interactive launches so Codex does not warn about daemon fallback.
// Keep remote connections and other subcommands on their existing paths.
func codexNeedsEmbeddedFlag(args []string) bool {
	positional := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		switch arg {
		case "--no-daemon", "--remote", "--help", "-h", "--version", "-V":
			return false
		case "-c", "--config", "-C", "--cd", "-m", "--model", "-p", "--profile",
			"-s", "--sandbox", "-a", "--ask-for-approval", "-i", "--image",
			"--local-provider", "--add-dir", "--enable", "--disable":
			i++ // A flag value is neither a command nor another option.
			continue
		}
		if strings.HasPrefix(arg, "--remote=") || strings.HasPrefix(arg, "--no-daemon=") {
			return false
		}
		if strings.HasPrefix(arg, "-") || positional {
			continue
		}
		positional = true
		switch arg {
		case "exec", "e", "review", "agents", "login", "logout", "mcp", "plugin",
			"app-server", "remote-control", "app", "completion", "update", "doctor",
			"sandbox", "debug", "execpolicy", "apply", "a", "queue", "archive",
			"delete", "migrate-rollouts", "unarchive", "cloud", "cloud-tasks",
			"responses-api-proxy", "stdio-to-uds", "tcp-tunnel", "exec-server", "features", "help":
			return false
		}
		// resume, fork, and a positional prompt are interactive.
	}
	return true
}

// Compatibility resolution owns version rules, probes, and their persistent cache.
func codexSupportsNoDaemon() bool {
	binary, err := RealBinary(AgentCodex)
	if err != nil {
		return false
	}
	profile := compat.Resolve(context.Background(), compat.Installation{Harness: AgentCodex, Surface: compat.CLI, Path: binary})
	return profile.Capability(compat.CodexNoDaemon).Support == compat.Supported
}
