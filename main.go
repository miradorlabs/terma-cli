// Command terma connects coding agents to Terma and stamps the commits they produce.
// Everything is in package cmd; this is only the process boundary, where the command
// tree's result becomes an exit status.
package main

import (
	"os"

	"github.com/miradorlabs/terma-cli/cmd"
	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
)

func main() { os.Exit(cmd.Execute(builtin.Agents())) }
