package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/dispatch"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/repohooks"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// HooksDisabled reports the kill switch `TERMA_HOOKS=0`, which makes every hook exit 0 at once.
func HooksDisabled() bool {
	return os.Getenv("TERMA_HOOKS") == "0"
}

func (app *App) newHookCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "hook <event> [args...]",
		Short:  "Internal: the runtime behind every installed hook shim",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		// Hooks exit 0 whatever happens, on top of the shims' `|| true`.
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, _ := os.Getwd()
			status := dispatch.Run(cmd.Context(), app.hookDeps(), dispatch.Request{
				Event: args[0], Args: args[1:],
				Stdin: cmd.InOrStdin(), Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr(),
				Version: app.version, Debug: os.Getenv("TERMA_DEBUG") != "", HooksOff: HooksDisabled(), Cwd: cwd,
			})
			if status != 0 {
				return exitWith(status)
			}
			return nil
		},
	}
	// --user is ignored here: it marks the entries setup writes, so terma recognizes its own
	// in an agent's hooks file (hookmgr's userHookShape, ManagedDeployed).
	cmd.Flags().Bool("user", false, "internal: a machine-wide hook entry")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// hookDeps are what `terma hook` reaches beyond the hook runtime.
func (app *App) hookDeps() dispatch.Deps {
	return dispatch.Deps{
		ConfigDir: app.dir,
		StateDir:  app.stateDir,
		Agents:    app.agents,
		Profile:   app.hookProfile,
		Spool:     app.openSpool,
		ClaimedRouted: func(cwd string, rt *hookrun.Routing) {
			app.claimed(cwd, func(gitDir string) bool { return rt.GitHooks(gitDir, time.Now()) })
		},
		Flush:     spawnFlush,
		AwaitPush: func(path string) { spawnDetached("spool", "await-push", path) },
	}
}

func (app *App) claimed(cwd string, wanted func(gitDir string) bool) {
	daemon.Spawn(app.stateDir, app.version)
	terma, err := app.hookExecutable()
	if err == nil {
		_, err = repohooks.SyncIf(app.stateDir, terma, wanted, cwd)
	}
	if err != nil && os.Getenv("TERMA_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "terma hook: this repository's commit hooks: %v\n", err)
	}
}

// hookProfile reads config.json, sign-in, and routing/<profile>.json as one snapshot;
// missing, mid-read changed, corrupt, or wrong-account routing state fails closed with
// zero fallback to Profile.Team.
func (app *App) hookProfile() dispatch.Profile {
	p := dispatch.Profile{Policy: config.NoPolicy("", ""), Routing: hookrun.NewRouting("", nil)}
	lane, _ := readLaneAsOne(app.dir, func() string {
		cfg, err := config.Load(app.dir, app.stateDir, config.Overrides{})
		if err != nil {
			return sendas.AccountUnreadable
		}
		p = dispatch.Profile{Agents: cfg.Harnesses, Policy: config.NoPolicy("", ""), Routing: hookrun.NewRouting(cfg.ProfileName, nil)}
		rpath, rperr := routing.StatePath(app.stateDir, cfg.ProfileName)
		before := statOrNil(rpath)
		lane := app.profileLane(cfg)
		st, found, rerr := routing.Load(app.stateDir, cfg.ProfileName)
		stable := rperr == nil && unchanged(before, statOrNil(rpath))
		if stable && found && rerr == nil && lane != "" && lane != sendas.AccountUnreadable && st.Account == lane {
			p.Routing = hookrun.NewRouting(cfg.ProfileName, &st)
			return lane
		}
		if stable && lane == "" {
			return ""
		}
		return sendas.AccountUnreadable
	})
	if lane == sendas.AccountUnreadable {
		p.Routing = hookrun.NewRouting(p.Routing.Profile, nil)
	}
	if p.ServerKey = lane == sendas.NoPersonLane; !p.ServerKey {
		p.Account = lane
	}
	if p.Routing.Usable() && daemon.Running(claim.Dir(app.stateDir)) {
		if info, ok := daemon.RunningRelay(claim.Dir(app.stateDir)); !ok || !info.Routing {
			if ok {
				daemon.Supersede(app.stateDir, app.version)
			}
			p.Routing.WithholdPlacements = true
		}
	}
	return p
}

// profileLane is the lane of cfg's profile, which was just read from config.json: no
// person's under a server key, else its sign-in's account as auth.Account names it, paired
// with the profile (pairedLane).
func (app *App) profileLane(cfg *config.Config) string {
	if cfg.ServerKeySignIn {
		return sendas.NoPersonLane
	}
	if app.betweenProfileReads != nil {
		app.betweenProfileReads()
	}
	return pairedLane(auth.Account(cfg.Dir, cfg.ProfileName, cfg.AuthURL), cfg.Account)
}

// hookPolicy is the collection policy while it is validated and fresh, else NoPolicy.
func (app *App) hookPolicy() config.Policy {
	cfg, err := config.Load(app.dir, app.stateDir, config.Overrides{})
	if err != nil {
		return config.NoPolicy("", "")
	}
	if !cfg.Policy.Validated() || cfg.Policy.Expired(time.Now()) {
		return config.NoPolicy(cfg.OrganizationID, cfg.AuthURL)
	}
	return cfg.Policy
}

// openSpool returns nil when the state directory cannot be used: a nil spool drops events,
// never failing a hook.
func (app *App) openSpool() *spool.Spool {
	s, err := spool.Open(filepath.Join(app.stateDir, spool.Dir))
	if err != nil {
		return nil
	}
	return s
}

// spawnFlush is silent on failure: the next flush picks up whatever this one leaves.
func spawnFlush() { spawnDetached("spool", "flush", "--quiet") }

// spawnDetached starts terma with args, detached from the hook that asks; it is silent on
// failure.
func spawnDetached(args ...string) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	proc := exec.Command(exe, args...)
	proc.Stdin, proc.Stdout, proc.Stderr = nil, nil, nil
	procinfo.Detach(proc)
	if err := proc.Start(); err != nil {
		return
	}
	_ = proc.Process.Release()
}
