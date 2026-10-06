package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// primaryCommands is terma's whole surface (MIR-80): what `terma --help` lists, and the
// only commands a message may tell someone to run. Each is safe to run again.
var primaryCommands = []string{
	"doctor", "setup", "teardown", "update",
}

// advancedCommands are hidden: other programs run them (hook, relay, spool, version),
// Terma's engineers do (config, nate), or the e2e suites still do (status; MIR-80 removes
// it once those move to the primary commands).
var advancedCommands = []string{
	"config", "hook", "nate", "relay", "spool", "status", "version",
}

func commandNamed(root *cobra.Command, name string) *cobra.Command {
	for _, c := range root.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

func TestHelpListsOnlyThePrimaryCommands(t *testing.T) {
	var visible []string
	for _, c := range testApp.NewRootCommand().Commands() {
		// cobra's own `help` and `completion` are not terma's to list or hide.
		if c.IsAvailableCommand() && c.Name() != "help" && c.Name() != "completion" {
			visible = append(visible, c.Name())
		}
	}
	slices.Sort(visible)
	if !slices.Equal(visible, primaryCommands) {
		t.Fatalf("`terma --help` lists\n  %v\nwant\n  %v\nA new command is advanced unless a developer needs it day to day: give it `Hidden: true` and add it to advancedCommands.", visible, primaryCommands)
	}

	listing, _, err := termaRun{}.exec(t, "--help")
	if err != nil {
		t.Fatal(err)
	}
	if i := strings.Index(listing, "Available Commands:"); i >= 0 {
		listing = listing[i:]
	}
	if j := strings.Index(listing, "Flags:"); j >= 0 {
		listing = listing[:j]
	}
	for _, name := range append(slices.Clone(advancedCommands), "completion") {
		if regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(name) + `\s`).MatchString(listing) {
			t.Errorf("`terma --help` lists the advanced command %q:\n%s", name, listing)
		}
	}
}

// Every advanced command is still there, hidden, and answers `--help`, the cheapest proof
// that it is wired.
func TestAdvancedCommandsAreHiddenNotRemoved(t *testing.T) {
	useConfigDir(t, t.TempDir())
	root := testApp.NewRootCommand()
	var known []string
	for _, c := range root.Commands() {
		known = append(known, c.Name())
	}
	for _, name := range advancedCommands {
		c := commandNamed(root, name)
		if c == nil {
			t.Errorf("%q is gone; it was meant to be hidden, not removed (have %v)", name, known)
			continue
		}
		if !c.Hidden {
			t.Errorf("%q is advanced and must be `Hidden: true`", name)
		}
		if out, err := runTerma(t, name, "--help"); err != nil || out == "" {
			t.Errorf("`terma %s --help` = %v, output %q", name, err, out)
		}
	}
	// Every command is one or the other: nothing is left unclassified.
	for _, name := range known {
		if name == "help" || name == "completion" {
			continue
		}
		if !slices.Contains(primaryCommands, name) && !slices.Contains(advancedCommands, name) {
			t.Errorf("%q is in neither primaryCommands nor advancedCommands", name)
		}
	}
}

// namedCommand requires the closing backtick on the same line, so a raw string that begins
// "terma connects …" is not read as a command called "connects".
var namedCommand = regexp.MustCompile("`terma ([a-z][a-z-]*)[^`\n]*`")

// unquotedCommand finds the two unquoted shapes: a doctor `Fix: "terma install"` and a
// sentence ending `with: terma doctor`.
var unquotedCommand = regexp.MustCompile(`(?:Fix:\s*"|[Ww]ith: )terma ([a-z][a-z-]*)`)

// Every `terma <name>` the shipped source and docs name must be a command that exists,
// hidden or not.
func TestEveryCommandAMessageNamesExists(t *testing.T) {
	root := testApp.NewRootCommand()
	exists := map[string]bool{"help": true, "completion": true}
	for _, c := range root.Commands() {
		exists[c.Name()] = true
		for _, alias := range c.Aliases {
			exists[alias] = true
		}
	}
	shipped := func(path string) bool {
		switch {
		case strings.HasSuffix(path, ".go"):
			return !strings.HasSuffix(path, "_test.go")
		case strings.HasSuffix(path, ".md"):
			return true
		}
		return false
	}
	for _, dir := range []string{filepath.Join("..", "..", "cmd"), "..", filepath.Join("..", "..", "docs"), filepath.Join("..", "..", "README.md")} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !shipped(path) {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(data), "\n") {
				named := append(namedCommand.FindAllStringSubmatch(line, -1), unquotedCommand.FindAllStringSubmatch(line, -1)...)
				for _, m := range named {
					if !exists[m[1]] {
						t.Errorf("%s:%d tells someone to run `terma %s`, which is not a command: %s", path, i+1, m[1], strings.TrimSpace(line))
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// Completion is hidden, not switched off: the Homebrew cask runs `terma completion <shell>`
// during install, and a failure there fails the install.
func TestCompletionIsHiddenNotRemoved(t *testing.T) {
	out, err := runTerma(t, "completion", "zsh")
	if err != nil || !strings.Contains(out, "compdef") {
		t.Fatalf("`terma completion zsh` = %v, output %.200q", err, out)
	}
}

// hiddenCommandSources implement a hidden command whose own help names it. Every other
// shipped line a person reads names only a primary command.
var hiddenCommandSources = []string{
	"internal/cli/config.go", "internal/relay/service/windows.go",
}

// The fix-it hints, errors and help a developer reads name one of the six commands, so
// nobody is sent to a command they were never meant to learn.
func TestMessagesNameOnlyPrimaryCommands(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, src := range hiddenCommandSources {
		if _, err := os.Stat(filepath.Join(root, src)); err != nil {
			t.Errorf("hiddenCommandSources names %s: %v", src, err)
		}
	}
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if slices.Contains(hiddenCommandSources, rel) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			named := append(namedCommand.FindAllStringSubmatch(line, -1), unquotedCommand.FindAllStringSubmatch(line, -1)...)
			for _, m := range named {
				// `terma hook` is named to say what agents and git run, never as advice.
				if !slices.Contains(primaryCommands, m[1]) && m[1] != "hook" {
					t.Errorf("%s:%d tells someone to run `terma %s`, which is not one of %v: %s", rel, i+1, m[1], primaryCommands, strings.TrimSpace(line))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
