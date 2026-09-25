package cmd

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

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

func newUpdateCommand() *cobra.Command {
	var check, force, refresh bool
	var automatic string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Check for updates, install the latest release, or configure automatic updates",
		Long: `Downloads the latest published release for this platform, verifies its checksum,
and replaces the binary in place. Existing hooks continue to work.

A Homebrew or npm installation is upgraded through that package manager instead —
the one that owns this copy of terma, not whichever one is first on PATH.

After the new version is in place it refreshes what earlier versions wrote — the agent
shims, the wrapped Claude Code status line, the OpenCode plugin, and the hooks of the
repository you run it in — keeping every choice you made at install time. It works from
what is on disk: it signs in to nothing and never adds a file. --refresh runs just that
step; inside each other repository terma is installed in, run it to update the hooks
there (they are committed files, so they change only when you ask).

Normal interactive commands check daily and notify you when a newer version exists.
Use --auto on to install those updates automatically, or --auto off for notices only.
Automatic updates run after successful interactive commands, never inside agent hooks,
launch shims, scripts, or CI. Package-managed installations receive notices only.

Updates compare the installed release version with the latest published release.
Source builds are not updated automatically; use --force to switch one to the latest
release.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if refresh {
				return runRefresh(cmd.Context(), out)
			}
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("auto") {
				switch automatic {
				case "on", "off":
					if err := selfupdate.SavePreferences(dir, selfupdate.Preferences{Auto: automatic == "on"}); err != nil {
						return err
					}
					if automatic == "on" {
						fmt.Fprintln(out, "Automatic updates enabled for numbered releases after interactive commands. Package-managed installations receive notices only; unversioned development builds are skipped.")
					} else {
						fmt.Fprintln(out, "Automatic updates disabled; update notices remain enabled.")
					}
				case "status":
					p, err := selfupdate.LoadPreferences(dir)
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
			unlock, err := selfupdate.Lock(dir)
			if err != nil {
				return fmt.Errorf("cannot start update (another check or update may be running): %w", err)
			}
			defer unlock()
			return runUpdate(cmd.Context(), &selfupdate.Client{Version: Version}, dir, exe, out, check, force)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "check for a newer published release without installing")
	cmd.Flags().BoolVar(&force, "force", false, "replace a source/development build with the latest published release")
	cmd.Flags().StringVar(&automatic, "auto", "", "automatic updates: on, off, or status (default: off)")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "only refresh what terma installed (shims, status line, OpenCode plugin, this repository's hooks) to this version; runs by itself after an update")
	cmd.MarkFlagsMutuallyExclusive("auto", "check", "force", "refresh")
	return cmd
}

// These bound the parts of an update: the release lookup and download, a package
// manager (Homebrew updates its taps first, which can take minutes), and the new
// binary's refresh.
const (
	downloadTimeout = 2 * time.Minute
	upgradeTimeout  = 15 * time.Minute
	refreshTimeout  = time.Minute
)

func runUpdate(ctx context.Context, client *selfupdate.Client, dir, exe string, out io.Writer, check, force bool) error {
	current := client.Version
	download, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	rel, err := client.Latest(download)
	if errors.Is(err, selfupdate.ErrNoRelease) {
		selfupdate.SaveCache(dir, selfupdate.Cache{CheckedAt: time.Now(), Current: current, Failed: true})
		if check {
			fmt.Fprintln(out, err.Error()+". No binary update is available.")
			return nil
		}
		return err
	}
	if err != nil {
		cache := selfupdate.LoadCache(dir)
		cache.CheckedAt, cache.Current, cache.Failed = time.Now(), current, true
		selfupdate.SaveCache(dir, cache)
		return err
	}
	selfupdate.SaveCache(dir, selfupdate.Cache{CheckedAt: time.Now(), Current: current, Latest: rel.Version()})
	if !selfupdate.IsRelease(current) {
		if check {
			fmt.Fprintf(out, "terma %s is a development build; latest release is %s. Use `terma update --force` to switch to it.\n", current, rel.Version())
			return nil
		}
		if !force {
			return errors.New("this is a source/development build; use `terma update --force` to replace it with a published release")
		}
	} else if !selfupdate.Newer(current, rel.TagName) {
		fmt.Fprintf(out, "terma %s is up to date (latest %s).\n", current, rel.Version())
		return nil
	}
	if check {
		fmt.Fprintf(out, "terma %s → %s available.\n", current, rel.Version())
		return nil
	}
	if m, ok := selfupdate.ManagedBy(exe); ok {
		return upgradeManaged(ctx, m, current, rel.Version(), out)
	}
	if runtime.GOOS == "windows" {
		return errors.New("download the latest release from https://github.com/" + selfupdate.Repo + "/releases; in-place updates on Windows are not supported yet")
	}
	installed, err := client.Apply(download, rel, exe, out)
	if err != nil {
		return err
	}
	selfupdate.SaveCache(dir, selfupdate.Cache{CheckedAt: time.Now(), Current: installed, Latest: installed})
	fmt.Fprintf(out, "Updated terma %s → %s (%s).\n", current, installed, exe)
	finishUpdate(ctx, exe, out)
	return nil
}

// upgradeManaged upgrades a package-managed installation with the package manager that
// owns it, then has the upgraded binary finish the update. When that package manager
// cannot be found, or it fails, the developer is given the command to run.
func upgradeManaged(ctx context.Context, m selfupdate.Manager, current, latest string, out io.Writer) error {
	if len(m.Argv) == 0 {
		return fmt.Errorf("this installation is managed by %s; run `%s`", m.Name, m.Command)
	}
	fmt.Fprintf(out, "Upgrading terma %s → %s with %s: %s\n", current, latest, m.Name, strings.Join(m.Argv, " "))
	ctx, cancel := context.WithTimeout(ctx, upgradeTimeout)
	defer cancel()
	if err := runUpdateStep(ctx, out, m.Argv...); err != nil {
		return fmt.Errorf("%s could not upgrade terma (%w); run `%s` yourself", m.Name, err, m.Command)
	}
	finishUpdate(ctx, m.Terma, out)
	return nil
}

// finishUpdate has the new binary refresh what earlier versions wrote. This process is
// still the old version, so the refresh must run in the new one. The update itself has
// succeeded either way; a refresh that fails says how to retry.
func finishUpdate(ctx context.Context, terma string, out io.Writer) {
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	fmt.Fprintln(out)
	if err := runUpdateStep(ctx, out, terma, "update", "--refresh"); err != nil {
		fmt.Fprintf(out, "The new version is installed, but refreshing what terma installed failed (%v). Run `terma update --refresh` to retry.\n", err)
	}
}

// runUpdateStep runs one program of an update — a package manager, or the new terma
// finishing it — attached to the terminal, so a password prompt or progress reaches the
// developer. Tests replace it.
var runUpdateStep = func(ctx context.Context, out io.Writer, argv ...string) error {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, out, os.Stderr
	return c.Run()
}
