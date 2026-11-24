// Command benchhooks installs the commit hooks a claimed agent session installs, through
// the same code, for scripts/bench-hook.sh: the settings it reads come from the config
// directory and the record it writes from the state directory (in the bench's sandbox
// TERMA_CONFIG_DIR sets both), and the arguments are the terma the hooks run and the
// checkout to install into.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/repohooks"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

func main() {
	routed := flag.Bool("routed", false, "write routing.State before installing hooks")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: benchhooks [-routed] <terma> <checkout>")
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
	now, changed := time.Now(), false
	if *routed {
		pol := cfg.Policy
		pol.OrganizationID, pol.AuthURL = "org_bench", cfg.AuthURL
		_, err = routing.Create(stateDir, cfg.ProfileName, sendas.NoPersonLane, func(s *routing.State) error {
			if _, err := s.Select(routing.Team{OrganizationID: "org_bench", AuthURL: cfg.AuthURL, TeamID: "proj_bench", Policy: pol}); err != nil {
				return err
			}
			return s.Bind("github.com/acme/repo", "org_bench", false)
		})
		if err == nil {
			rt := hookrun.LoadRouting(stateDir, cfg.ProfileName)
			changed, err = repohooks.SyncIf(stateDir, flag.Arg(0), func(gitDir string) bool { return rt.GitHooks(gitDir, now) }, flag.Arg(1))
		}
	} else {
		changed, err = repohooks.Sync(stateDir, flag.Arg(0), cfg.Policy, now, flag.Arg(1))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if !changed {
		fmt.Fprintln(os.Stderr, "the policy installed no hooks in "+flag.Arg(1))
		os.Exit(1)
	}
}
