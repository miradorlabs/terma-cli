package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/hooks/dispatch"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/repohooks"
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
		Claimed: func(cwd string, policy config.Policy) {
			daemon.Spawn(app.stateDir, app.version)
			// The repository's commit hooks are installed here, the first time a session is
			// claimed in it while the policy asks for them.
			terma, err := app.hookExecutable()
			if err == nil {
				// The record of what was installed where is runtime state, like the spool.
				_, err = repohooks.Sync(app.stateDir, terma, policy, time.Now(), cwd)
			}
			// A hook never fails on this, but a developer debugging one should see why a
			// repository has no hooks, or a half-finished install.
			if err != nil && os.Getenv("TERMA_DEBUG") != "" {
				fmt.Fprintf(os.Stderr, "terma hook: this repository's commit hooks: %v\n", err)
			}
		},
		Flush:     spawnFlush,
		AwaitPush: func(path string) { spawnDetached("spool", "await-push", path) },
	}
}

// hookProfile is one small local read. Hooks run on the last validated policy even once
// it has expired, so a refresh outage stops no claim: what leaves is decided downstream,
// under the policy in force then.
func (app *App) hookProfile() dispatch.Profile {
	cfg, err := config.Load(app.dir, app.stateDir, config.Overrides{})
	if err != nil {
		// Bound to no account, but NoPolicy admits no repository, so nothing is spooled or
		// parked under it: an emit path that skipped Repo's admission would queue events a
		// later server key sends.
		return dispatch.Profile{Policy: config.NoPolicy("", "")}
	}
	p := dispatch.Profile{Team: cfg.Policy.TeamID, Agents: cfg.Harnesses, Policy: cfg.Policy.InForce(cfg.OrganizationID, cfg.AuthURL)}
	if p.ServerKey = cfg.ServerKeySignIn; !p.ServerKey {
		p.Account = auth.Account(cfg.Dir, cfg.ProfileName, cfg.AuthURL)
		p.AccountSince = accountSince(cfg, p.Account)
	}
	// Read once per hook run, and only by a replay that needs it.
	p.LegacyBefore = sync.OnceValue(func() time.Time { return legacyBefore(app.dir, app.stateDir) })
	return p
}

// legacyBefore is when an older terma's sign-ins were retired: what has no account from
// before then is theirs. While they are still on the machine (a retirement that could not
// forget them, or none yet), or the record can't be read, every event so far is.
func legacyBefore(dir, stateDir string) time.Time {
	rec, err := auth.ReadLegacyRecord(stateDir)
	switch {
	case err != nil, auth.HasLegacySignIns(dir):
		return farFuture
	}
	return rec.ReplayBefore()
}

// accountSince is when account, the profile's sign-in, began, which what unparking binds to
// it is from since. The unreadable account takes every unbound event, from any time: it
// sends none, and keeps them from being no one's, which a server key would send.
func accountSince(cfg *config.Config, account string) time.Time {
	if account == auth.AccountUnreadable {
		return time.Unix(0, 0)
	}
	return auth.SignedInAt(cfg.Dir, cfg.ProfileName)
}

// farFuture is a boundary nothing is after.
var farFuture = auth.LegacyStillSignedIn

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
