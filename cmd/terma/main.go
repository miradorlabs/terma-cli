// Command terma connects coding agents to Terma and stamps the commits they produce.
// The command line is internal/cli; this is only the process boundary, where it is
// handed the agents this build registers and its version, and its result becomes an
// exit status.
package main

import (
	"os"

	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
	"github.com/miradorlabs/terma-cli/internal/cli"
)

// version is stamped at build time via -ldflags: the release tag by GoReleaser, `git
// describe` by `make build`. "dev" is the unset sentinel.
var version = "dev"

func main() { os.Exit(cli.New(builtin.Agents(), version).Execute()) }
