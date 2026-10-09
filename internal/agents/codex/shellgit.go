package codex

import (
	"path/filepath"
	"slices"
	"strings"
)

// committing are the git subcommands that make commits prepare-commit-msg stamps.
var committing = []string{"commit", "cherry-pick", "revert", "rebase", "am"}

func makesCommit(words []word) bool {
	command, _ := gitCommand(words, "")
	if len(command) == 0 {
		return false
	}
	name := command[0].text
	if name != "merge" && !slices.Contains(committing, name) {
		return false
	}
	for _, flag := range []string{"--abort", "--quit", "--no-commit", "--dry-run"} {
		if hasGitOption(command, flag, "") {
			return false
		}
	}
	if name == "merge" && hasGitOption(command, "--squash", "") {
		return false
	}
	if (name == "cherry-pick" || name == "revert") && hasGitOption(command, "--no-commit", "n") {
		return false
	}
	return true // an unstamped merge still retires the files it incorporates
}

// hasGitOption ignores option values and pathspecs, and honors --no-<option> toggles.
func hasGitOption(args []word, flag, short string) bool {
	enabled := false
	opposite := "--no-" + strings.TrimPrefix(flag, "--")
	if strings.HasPrefix(flag, "--no-") {
		opposite = "--" + strings.TrimPrefix(flag, "--no-")
	}
	for i := 1; i < len(args); i++ {
		a := args[i].text
		if a == "--" {
			break
		}
		if (a == "--squash" && args[0].text == "commit") || slices.Contains([]string{"-m", "--message", "-F", "--file", "-c", "-C", "--reuse-message", "--reedit-message", "--author", "--date", "--fixup", "--trailer", "-t", "--template"}, a) {
			i++
			continue
		}
		if a == flag || (short != "" && strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Trim(a[1:], "fvknxesq") == "" && strings.Contains(a[1:], short)) {
			enabled = true
		} else if a == opposite {
			enabled = false
		}
	}
	return enabled
}

// gitCommand strips Git's global options and resolves each -C relative to the previous
// one. A work-tree override or expanded directory leaves relative operands unknown.
func gitCommand(words []word, cwd string) ([]word, string) {
	if len(words) == 0 || filepath.Base(words[0].text) != "git" {
		return nil, cwd
	}
	chdir := func(w word) {
		switch {
		case !w.literal:
			cwd = ""
		case w.text == "": // git -C '' keeps its current directory
		case filepath.IsAbs(w.text):
			cwd = filepath.Clean(w.text)
		case cwd != "":
			cwd = filepath.Join(cwd, w.text)
		}
	}
	for i := 1; i < len(words); i++ {
		switch a := words[i].text; {
		case a == "-C":
			i++
			if i >= len(words) {
				return nil, ""
			}
			chdir(words[i])
		case strings.HasPrefix(a, "-C"):
			chdir(word{strings.TrimPrefix(a, "-C"), words[i].literal})
		case a == "--git-dir" || a == "--work-tree":
			cwd = ""
			i++
		case strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree="):
			cwd = ""
		case a == "-c" || a == "--namespace":
			i++
		case !strings.HasPrefix(a, "-"):
			return words[i:], cwd
		}
	}
	return nil, cwd
}
