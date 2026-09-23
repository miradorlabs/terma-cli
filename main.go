// Command terma connects coding agents to Terma and stamps the commits they produce.
// Everything is in package cmd; this is only the process boundary, where the command
// tree's result becomes an exit status.
package main

import (
	"os"

	"github.com/miradorlabs/terma-cli/cmd"
)

func main() { os.Exit(cmd.Execute()) }
