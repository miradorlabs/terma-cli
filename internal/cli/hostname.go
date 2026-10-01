package cli

import (
	"os"
	"strings"
)

// hostname is the fallback identity when git has no email configured, and the default
// suffix for a minted key's name.
func hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "unknown-host"
	}
	if short, _, found := strings.Cut(name, "."); found && short != "" {
		return short
	}
	return name
}
