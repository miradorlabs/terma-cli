// Command benchhooks installs the commit hooks a claimed agent session installs, through
// the same code, for scripts/bench-hook.sh: the settings it reads come from the config
// directory and the record it writes from the state directory (in the bench's sandbox
// TERMA_CONFIG_DIR sets both), and the arguments are the terma the hooks run and the
// checkout to install into.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/repohooks"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: benchhooks <terma> <checkout>")
		os.Exit(2)
	}
	dir, err := config.Dir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	stateDir, err := config.StateDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg, err := config.Load(dir, stateDir, config.Overrides{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	changed, err := repohooks.Sync(stateDir, os.Args[1], cfg.Policy, time.Now(), os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if !changed {
		fmt.Fprintln(os.Stderr, "the policy installed no hooks in "+os.Args[2])
		os.Exit(1)
	}
}
