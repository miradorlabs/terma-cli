// Command benchhooks installs the commit hooks a claimed agent session installs, through
// the same code, for scripts/bench-hook.sh: the settings it reads come from the config
// directory and the record it writes from the state directory (in the bench's sandbox
// TERMA_CONFIG_DIR sets both), and the arguments are the terma the hooks run and the
// checkout to install into. With -migrated it first moves the sandbox to routing by teams,
// as the relay's migration leaves a single-team machine: marked migrated, with a committed
// routing state selecting the team, its repository bound to its organization, and installs
// the hooks where routing sends the checkout to a team with git_hooks on.
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
)

// The migrated sandbox's team, its organization and the repository its policy lists.
const (
	benchTeam = "proj_bench"
	benchOrg  = "org_bench"
	benchRepo = "github.com/acme/repo"
)

func main() {
	migrated := flag.Bool("migrated", false, "move the sandbox to routing by teams first")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: benchhooks [-migrated] <terma> <checkout>")
		os.Exit(2)
	}
	terma, checkout := flag.Arg(0), flag.Arg(1)
	if err := run(terma, checkout, *migrated); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(terma, checkout string, migrated bool) error {
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	stateDir, err := config.StateDir()
	if err != nil {
		return err
	}
	cfg, err := config.Load(dir, stateDir, config.Overrides{})
	if err != nil {
		return err
	}
	now := time.Now()
	var changed bool
	if migrated {
		if err := migrate(dir, stateDir, cfg); err != nil {
			return err
		}
		rt := hookrun.LoadRouting(stateDir, cfg.ProfileName)
		changed, err = repohooks.SyncIf(stateDir, terma, func(gitDir string) bool { return rt.GitHooks(gitDir, now) }, checkout)
	} else {
		changed, err = repohooks.Sync(stateDir, terma, cfg.Policy, now, checkout)
	}
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("nothing installed hooks in %s", checkout)
	}
	return nil
}

// migrate marks the sandbox migrated and writes the profile's committed routing state.
func migrate(dir, stateDir string, cfg *config.Config) error {
	if err := os.WriteFile(config.CredentialsV2Path(dir), []byte("{}\n"), 0o600); err != nil {
		return err
	}
	pol := config.Policy{Mode: config.ModeRepo, Repositories: []string{benchRepo}, GitHooks: true, IncludePrompts: true,
		IncludeToolContent: true, Revision: 1, FetchedAt: time.Now().UTC(), TeamID: benchTeam, OrganizationID: benchOrg, AuthURL: cfg.AuthURL}
	_, err := routing.Create(stateDir, cfg.ProfileName, func(s *routing.State) error {
		id, err := s.Select(routing.Team{OrganizationID: benchOrg, AuthURL: cfg.AuthURL, TeamID: benchTeam, Policy: pol})
		if err != nil {
			return err
		}
		s.Migration = &routing.Migration{Phase: routing.Committed, Legacy: map[string]string{benchTeam: id}}
		return s.Bind(benchRepo, benchOrg, false)
	})
	return err
}
