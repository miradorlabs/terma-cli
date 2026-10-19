package cli

import "github.com/miradorlabs/terma-cli/internal/machine"

// hostname is the default suffix for a minted key's name.
func hostname() string {
	if name := machine.Name(); name != "" {
		return name
	}
	return "unknown-host"
}
