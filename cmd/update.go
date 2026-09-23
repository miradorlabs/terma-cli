package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

func newUpdateCommand() *cobra.Command {
	var check, force bool
	var automatic string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Check for updates, install the latest release, or configure automatic updates",
		Long: `Downloads the latest published release for this platform, verifies its checksum,
and replaces the binary in place. Existing hooks continue to work.

Normal interactive commands check daily and notify you when a newer version exists.
Use --auto on to install those updates automatically, or --auto off for notices only.
Automatic updates run after successful interactive commands, never inside agent hooks,
launch shims, scripts, or CI. Homebrew/npm installations use their package manager.

Updates compare the installed release version with the latest published release.
Source builds are not updated automatically; use --force to switch one to the latest
release.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
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
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			unlock, err := selfupdate.Lock(dir)
			if err != nil {
				return fmt.Errorf("cannot start update (another check or update may be running): %w", err)
			}
			defer unlock()
			return runUpdate(ctx, &selfupdate.Client{Version: Version}, dir, exe, out, check, force)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "check for a newer published release without installing")
	cmd.Flags().BoolVar(&force, "force", false, "replace a source/development build with the latest published release")
	cmd.Flags().StringVar(&automatic, "auto", "", "automatic updates: on, off, or status (default: off)")
	cmd.MarkFlagsMutuallyExclusive("auto", "check", "force")
	return cmd
}

func runUpdate(ctx context.Context, client *selfupdate.Client, dir, exe string, out io.Writer, check, force bool) error {
	current := client.Version

	rel, err := client.Latest(ctx)
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
	if command := selfupdate.ManagedCommand(exe); command != "" {
		return fmt.Errorf("this installation is managed by a package manager; run `%s`", command)
	}
	if runtime.GOOS == "windows" {
		return errors.New("download the latest release from https://github.com/" + selfupdate.Repo + "/releases; in-place updates on Windows are not supported yet")
	}
	installed, err := client.Apply(ctx, rel, exe, out)
	if err != nil {
		return err
	}
	selfupdate.SaveCache(dir, selfupdate.Cache{CheckedAt: time.Now(), Current: installed, Latest: installed})
	fmt.Fprintf(out, "Updated terma %s → %s (%s).\n", current, installed, exe)
	return nil
}
