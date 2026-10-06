// Command terma connects coding agents to Terma and stamps the commits they produce;
// the command line itself is internal/cli.
package main

import (
	"os"

	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
	"github.com/miradorlabs/terma-cli/internal/cli"
)

// version is stamped via -ldflags (the release tag, or `git describe`); "dev" when unset.
var version = "dev"

func main() { os.Exit(cli.New(builtin.Agents, version).Execute()) }
