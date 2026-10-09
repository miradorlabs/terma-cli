package codex

import (
	"path/filepath"
	"slices"
	"strings"
)

// committing are the git subcommands that make commits prepare-commit-msg stamps.
var committing = []string{"commit", "cherry-pick", "revert", "rebase", "am"}

func makesCommit(words []word) bool {
	if gitSubcommand(words) != "merge" {
		return slices.Contains(committing, gitSubcommand(words))
	}
	for _, w := range words {
		if slices.Contains([]string{"--abort", "--quit", "--squash", "--no-commit"}, w.text) {
			return false
		}
	}
	return true // an unstamped merge still retires the files it incorporates
}

// gitSubcommand is the subcommand of a git command line, after git's own options.
func gitSubcommand(words []word) string {
	command, _ := gitCommand(words, "")
	if len(command) > 0 {
		return command[0].text
	}
	return ""
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
