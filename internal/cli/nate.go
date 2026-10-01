package cli

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

const termaModulePath = "github.com/miradorlabs/terma-cli"

func (app *App) newNateCommand() *cobra.Command {
	var assumeYes bool
	cmd := &cobra.Command{
		Use:    "nate",
		Short:  "Remove Terma from this machine",
		Hidden: true,
		Args:   cobra.NoArgs,
		Long: `Restore user settings that Terma changed, remove machine routing and local
Terma state, then delete installed Terma executables. This is intended for testing
onboarding from a clean machine.

Repositories are left alone: their hooks and binding are committed files shared with
everyone who works in them. Remove a repository's install with 'terma uninstall'.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !assumeYes {
				ok, err := confirm(cmd, "Are you sure you want to do this? It will remove everything related to Terma on this machine.")
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(cmd.OutOrStdout(), "Cancelled. Nothing was removed.")
					return nil
				}
			}
			return app.runNate(cmd)
		},
	}
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

func (app *App) runNate(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()

	// Only home-directory state: a repository's committed wiring is `terma uninstall`'s.

	if err := app.undoSetup(cmd.Context(), out); err != nil {
		return err
	}

	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	if err := removeTermaConfigDir(configDir); err != nil {
		return err
	}
	fmt.Fprintf(out, "Removed local state from %s.\n", output.TildePath(configDir))

	paths := app.nateBinaryCandidates()
	for _, path := range paths {
		if err := app.nateRemoveBinary(cmd, path); err != nil {
			return fmt.Errorf("remove Terma executable %s: %w", path, err)
		}
		fmt.Fprintf(out, "Removed executable %s.\n", output.TildePath(path))
	}
	if len(paths) == 0 {
		fmt.Fprintln(out, "No installed Terma executable was found.")
	}
	fmt.Fprintln(out, "Terma has been removed. A new install will start with a fresh configuration.")
	fmt.Fprintln(out, "Repositories keep their committed hooks and binding; remove one with `terma uninstall` inside it.")
	return nil
}

func removeTermaConfigDir(path string) error {
	path = filepath.Clean(path)
	if path == "." || path == string(filepath.Separator) {
		return fmt.Errorf("refusing to remove unsafe Terma config directory %q", path)
	}
	if home, err := os.UserHomeDir(); err == nil && samePath(path, home) {
		return fmt.Errorf("refusing to remove the home directory as Terma's config directory: %s", path)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove Terma config directory %s: %w", path, err)
	}
	return nil
}

func samePath(a, b string) bool {
	a, errA := filepath.Abs(a)
	b, errB := filepath.Abs(b)
	return errA == nil && errB == nil && filepath.Clean(a) == filepath.Clean(b)
}

// installedTermaBinaries finds only executables built from this module (plus the npm
// launcher), sorting the running one last so a later failure can still be reported.
func (app *App) installedTermaBinaries() []string {
	name := "terma"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	current, _ := os.Executable()
	var candidates []string
	if filepath.Base(current) == name {
		candidates = append(candidates, current)
	}
	if path, err := exec.LookPath(name); err == nil {
		candidates = append(candidates, path)
	}
	for _, dir := range append(filepath.SplitList(os.Getenv("PATH")), app.binDirs()...) {
		if dir != "" {
			candidates = append(candidates, filepath.Join(dir, name))
		}
	}

	seen := map[string]bool{}
	paths := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate, err := filepath.Abs(candidate)
		if err != nil || seen[candidate] || !isTermaBinary(candidate) {
			continue
		}
		seen[candidate] = true
		paths = append(paths, candidate)
		// The npm command is a launcher beside a vendored binary; remove both.
		if target := doctor.InstalledBinary(candidate); target != candidate {
			if target, err = filepath.Abs(target); err == nil && !seen[target] && isTermaBinary(target) {
				seen[target] = true
				paths = append(paths, target)
			}
		}
	}
	sort.SliceStable(paths, func(i, j int) bool {
		return samePath(paths[j], current) && !samePath(paths[i], current)
	})
	return paths
}

func isTermaBinary(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || info.IsDir() || info.Mode()&(fs.ModeSymlink|0o111) == 0 {
		return false
	}
	build, err := buildinfo.ReadFile(doctor.InstalledBinary(path))
	return err == nil && build.Main.Path == termaModulePath
}

func removeNateBinary(cmd *cobra.Command, path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if !errors.Is(err, fs.ErrPermission) || !output.Interactive() || runtime.GOOS == "windows" {
		return err
	}
	// The install script uses /usr/local/bin when sudo is available; mirror it.
	sudo, lookErr := exec.LookPath("sudo")
	if lookErr != nil {
		return err
	}
	c := exec.CommandContext(cmd.Context(), sudo, "rm", "--", path)
	c.Stdin = cmd.InOrStdin()
	c.Stdout = cmd.OutOrStdout()
	c.Stderr = cmd.ErrOrStderr()
	if sudoErr := c.Run(); sudoErr != nil {
		return fmt.Errorf("%w (sudo removal also failed: %w)", err, sudoErr)
	}
	return nil
}
