package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

func (app *App) newUpdateCommand() *cobra.Command {
	var check, force, refresh bool
	var automatic string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Check for updates, install the latest release, or configure automatic updates",
		Long: `Downloads the latest published release for this platform, verifies its checksum,
and replaces the binary in place. Existing hooks continue to work.

A Homebrew or npm installation is upgraded through that package manager instead —
the one that owns this copy of terma, not whichever one is first on PATH.

After the new version is in place, update also refreshes what earlier versions wrote on
this machine — the agents' status line wrap and plugins, and the relay's service —
keeping every choice you made at setup. It works from what is on disk: it signs in to
nothing and never adds a file. Already on the latest release, update runs just that
refresh, so it is safe to run again.

terma checks daily for a newer release and installs it by itself: the local relay checks
in the background, so a machine nobody runs terma on still updates, and so do interactive
commands. Never inside agent hooks, scripts, or CI. Use --auto off for notices only, and
--auto on to go back to automatic updates. Homebrew and npm installations, and Windows,
receive notices only.

Updates compare the installed release version with the latest published release.
Source builds are not updated automatically; use --force to switch one to the latest
release.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if refresh {
				return app.runRefresh(cmd.Context(), out)
			}
			if cmd.Flags().Changed("auto") {
				switch automatic {
				case "on", "off":
					if err := selfupdate.SavePreferences(app.dir, selfupdate.Preferences{Auto: automatic == "on"}); err != nil {
						return err
					}
					if automatic == "on" {
						fmt.Fprintln(out, "Automatic updates enabled: terma installs each new release in the background. Package-managed installations receive notices only; unversioned development builds are skipped.")
					} else {
						fmt.Fprintln(out, "Automatic updates disabled; update notices remain enabled.")
					}
				case "status":
					p, err := selfupdate.LoadPreferences(app.dir)
					if err != nil {
						return err
					}
					mode := "off (notify only)"
					if p.Auto {
						mode = "on"
					}
					fmt.Fprintf(out, "Automatic updates: %s.\n", mode)
				default:
					return errors.New("--auto must be on, off, or status")
				}
				return nil
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			unlock, err := selfupdate.Lock(app.stateDir)
			if err != nil {
				return fmt.Errorf("cannot start update (another check or update may be running): %w", err)
			}
			defer unlock()
			return app.updateOrRefresh(cmd.Context(), app.updateClient(), app.stateDir, exe, out, check, force)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "check for a newer published release without installing")
	cmd.Flags().BoolVar(&force, "force", false, "replace a source/development build with the latest published release")
	cmd.Flags().StringVar(&automatic, "auto", "", "automatic updates: on, off, or status (default: on)")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "only refresh what terma installed on this machine (agents' status lines and plugins, the relay's service) to this version; runs by itself after an update")
	// The new binary runs `update --refresh` after an upgrade; people run plain `update`.
	_ = cmd.Flags().MarkHidden("refresh")
	cmd.MarkFlagsMutuallyExclusive("auto", "check", "force", "refresh")
	return cmd
}

// updatesSummary is how the terma at exe, at version, gets new releases: what setup says.
func updatesSummary(exe, version, goos string, auto bool) string {
	if m, ok := selfupdate.ManagedBy(exe); ok {
		return "through " + m.Name + ": `" + m.Command + "`"
	}
	switch {
	case !selfupdate.IsRelease(version):
		return "never for a development build: `terma update --force` installs the latest release"
	case goos == "windows":
		return "by hand: download each new release from https://github.com/" + selfupdate.Repo + "/releases"
	case !auto:
		return "when you run `terma update` (automatic updates are off: `terma update --auto on`)"
	}
	return "automatically: terma installs each new release in the background"
}

// Update phase bounds; a package manager may update its taps first, which takes minutes.
const (
	downloadTimeout = 2 * time.Minute
	upgradeTimeout  = 15 * time.Minute
	refreshTimeout  = time.Minute
)

// updateOrRefresh is `terma update`: when there is nothing newer to install, this version's
// refresh is the update, so running it again always brings what terma installed up to date.
// A failed check or install refreshes nothing: its error is the whole answer.
func (app *App) updateOrRefresh(ctx context.Context, client *selfupdate.Client, dir, exe string, out io.Writer, check, force bool) error {
	installing, err := app.runUpdate(ctx, client, dir, exe, out, check, force)
	if installing || check || err != nil {
		return err
	}
	return app.runRefresh(ctx, out)
}

// runUpdate reports whether it went on to install a newer version, whose own refresh then
// runs; false with no error means there was nothing newer to install.
func (app *App) runUpdate(ctx context.Context, client *selfupdate.Client, dir, exe string, out io.Writer, check, force bool) (bool, error) {
	// Under the caller's lock: the relay may have installed a release since this process
	// started, in which case its version says nothing about what is installed now.
	if client.Replaced(exe) {
		return false, errors.New("another install replaced terma while this command ran; run `terma update` again")
	}
	current := client.Version
	download, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	rel, err := client.Latest(download)
	if errors.Is(err, selfupdate.ErrNoRelease) {
		selfupdate.SaveCache(dir, selfupdate.Cache{CheckedAt: time.Now(), Current: current, Failed: true})
		if check {
			fmt.Fprintln(out, err.Error()+". No binary update is available.")
			return false, nil
		}
		return false, err
	}
	if err != nil {
		cache := selfupdate.LoadCache(dir)
		cache.CheckedAt, cache.Current, cache.Failed = time.Now(), current, true
		selfupdate.SaveCache(dir, cache)
		return false, err
	}
	selfupdate.SaveCache(dir, selfupdate.Cache{CheckedAt: time.Now(), Current: current, Latest: rel.Version(), Published: rel.PublishedAt})
	if !selfupdate.IsRelease(current) {
		if check {
			fmt.Fprintf(out, "terma %s is a development build; latest release is %s. Use `terma update --force` to switch to it.\n", current, rel.Version())
			return false, nil
		}
		if !force {
			fmt.Fprintf(out, "terma %s is a development build, so it is not replaced (`terma update --force` installs %s).\n", current, rel.Version())
			return false, nil
		}
	} else if !selfupdate.Newer(current, rel.TagName) {
		fmt.Fprintf(out, "terma %s is up to date (latest %s).\n", current, rel.Version())
		return false, nil
	}
	if check {
		fmt.Fprintf(out, "terma %s → %s available.\n", current, rel.Version())
		return false, nil
	}
	if m, ok := selfupdate.ManagedBy(exe); ok {
		return true, app.upgradeManaged(ctx, m, current, rel.Version(), out)
	}
	if runtime.GOOS == "windows" {
		return false, errors.New("download the latest release from https://github.com/" + selfupdate.Repo + "/releases; in-place updates on Windows are not supported yet")
	}
	installed, err := client.Apply(download, rel, exe, out)
	if err != nil {
		return false, err
	}
	selfupdate.SaveCache(dir, selfupdate.Cache{CheckedAt: time.Now(), Current: installed, Latest: installed, Published: rel.PublishedAt})
	fmt.Fprintf(out, "Updated terma %s → %s (%s).\n", current, installed, exe)
	app.finishUpdate(ctx, exe, out)
	return true, nil
}

// upgradeManaged runs the package manager that owns the installation, then the new
// binary's refresh; without that manager the developer is given the command.
func (app *App) upgradeManaged(ctx context.Context, m selfupdate.Manager, current, latest string, out io.Writer) error {
	if m.Project != "" {
		return fmt.Errorf("this terma is a dependency of the project in %s; run `%s` there", m.Project, m.Command)
	}
	if len(m.Argv) == 0 {
		return fmt.Errorf("this installation is managed by %s; run `%s`", m.Name, m.Command)
	}
	fmt.Fprintf(out, "Upgrading terma %s → %s with %s: %s\n", current, latest, m.Name, strings.Join(m.Argv, " "))
	ctx, cancel := context.WithTimeout(ctx, upgradeTimeout)
	defer cancel()
	if err := app.runUpdateStep(ctx, out, m.Argv...); err != nil {
		return fmt.Errorf("%s could not upgrade terma (%w); run `%s` yourself", m.Name, err, m.Command)
	}
	app.finishUpdate(ctx, m.Terma, out)
	return nil
}

// finishUpdate has the new binary run the refresh, since this process is still the old one.
func (app *App) finishUpdate(ctx context.Context, terma string, out io.Writer) {
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	fmt.Fprintln(out)
	if err := app.runUpdateStep(ctx, out, terma, "update", "--refresh"); err != nil {
		fmt.Fprintf(out, "The new version is installed, but refreshing what terma installed failed (%v). Run `terma update` to retry.\n", err)
	}
}

// runUpdateStep runs one update program attached to the terminal, so prompts reach the developer.
func runUpdateStep(ctx context.Context, out io.Writer, argv ...string) error {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, out, os.Stderr
	return c.Run()
}
