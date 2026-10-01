// Command terma connects coding agents to Terma and stamps the commits they produce.
// Everything is in package cmd; this is only the process boundary, where the command
// tree's result becomes an exit status.
package main

import (
	"os"

	"github.com/miradorlabs/terma-cli/cmd"
	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
)

// version is stamped at build time via -ldflags: the release tag by GoReleaser, `git
// describe` by `make build`. "dev" is the unset sentinel.
var version = "dev"

func main() { os.Exit(cmd.New(builtin.Agents(), version).Execute()) }
