package cmd

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

// primaryCommands is what `terma --help` lists. It is short on purpose: setup and
// install are the whole onboarding, status and doctor say whether it worked, and the
// rest read what it produced or keep the tool itself in order. A command added without
// `Hidden: true` lengthens this list and fails here — put it in advancedCommands instead,
// or add it to this list because a developer needs it in the normal course of things.
var primaryCommands = []string{
	"blame", "doctor", "install", "org", "session", "setup", "status", "uninstall", "update", "usage",
}

// advancedCommands are hidden, not removed. They are what automation, CI and
// troubleshooting run, and what terma's own fix-it hints name (`terma login`, `terma
// connect codex`, `terma spool flush`) — so every one of them must keep working.
var advancedCommands = []string{
	"config", "connect", "disconnect", "harness", "hook", "login", "logout",
	"principal", "project", "shim", "spool", "telemetry", "version", "whoami",
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
	for _, c := range NewRootCommand().Commands() {
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
	for _, name := range advancedCommands {
		if regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(name) + `\s`).MatchString(listing) {
			t.Errorf("`terma --help` lists the advanced command %q:\n%s", name, listing)
		}
	}
}

// Hidden is a statement about the help text and nothing else. Every advanced command is
// still there, still hidden, and still answers — `--help` on each is the cheapest proof
// that it is wired, and it runs nothing.
func TestAdvancedCommandsAreHiddenNotRemoved(t *testing.T) {
	t.Setenv("TERMA_CONFIG_DIR", t.TempDir())
	root := NewRootCommand()
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

// namedCommand finds a command terma tells someone to run: `terma <name> …`, quoted in
// backticks on one line — which is how every hint, error and help text writes it. (The
// closing backtick on the same line is what keeps a Go raw string that merely *begins*
// "terma connects your coding agents…" from reading as a command called "connects".)
var namedCommand = regexp.MustCompile("`terma ([a-z][a-z-]*)[^`\n]*`")

// unquotedCommand finds the two shapes a hint takes without backticks: a doctor check's
// `Fix: "terma install"`, and a sentence that ends in the command to run (`with: terma
// session list`). `terma connect` told everyone to run `terma trace list` — a command
// from the CLI this one was forked from — for as long as the hint stayed unquoted.
var unquotedCommand = regexp.MustCompile(`(?:Fix:\s*"|[Ww]ith: )terma ([a-z][a-z-]*)`)

// A message that names a command is a promise that running it works. `terma login` went
// on recommending `terma use` long after that command was removed, because nothing
// checked. This reads every hint, error, help text and comment in the shipped source and
// requires each `terma <name>` it finds to be a command that exists — hidden or not,
// which is also what stops a cleanup from removing a command the hints still name.
func TestEveryCommandAMessageNamesExists(t *testing.T) {
	root := NewRootCommand()
	exists := map[string]bool{"help": true, "completion": true}
	for _, c := range root.Commands() {
		exists[c.Name()] = true
		for _, alias := range c.Aliases {
			exists[alias] = true
		}
	}
	for _, dir := range []string{".", filepath.Join("..", "internal")} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
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
