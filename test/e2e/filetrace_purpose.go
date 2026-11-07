package e2e

import (
	"path/filepath"
	"slices"
	"strings"
)

// What a program the build started is for: the well-known command lines (execPurposes, the one
// table of them) and how a command line is read for them.

// isShell reports a shell's path or name.
func isShell(argv0 string) bool {
	switch filepath.Base(argv0) {
	case "sh", "bash", "zsh", "dash", "fish", "ksh":
		return true
	}
	return false
}

// execPurposes say what the well-known programs a build starts are for: an exec entry's
// purpose is the first rule's whose pattern its normalized command line matches. A rule that
// inherits gives its purpose to the programs the command starts that match none of their own
// (the cat a shell snapshot runs is the snapshot's); one that does not (a sandbox) says what the
// command itself does. A rule for tools says the same of a tool call's command and what it
// starts (a sandbox, a login shell's own startup files); the rest describe the build acting on
// its own and are not read into a tool's (the model's `git status` is not the build reading the
// repository for its context). A purpose is presentation, never compared between builds.
var execPurposes = []struct {
	pattern string // what it matches, for a reader
	match   func(argv []string) bool
	purpose string
	inherit bool
	tools   bool
}{
	{pattern: "a shell writing the shell snapshot: SNAPSHOT_FILE=… (Claude Code), print/echo '# Snapshot file' (Codex)",
		match: func(a []string) bool {
			s := shellScript(a)
			return strings.Contains(s, "SNAPSHOT_FILE=") || strings.Contains(s, "'# Snapshot file'")
		},
		purpose: "the shell snapshot: captures the login shell's environment for the shell tool", inherit: true},
	{pattern: "a shell loading a new snapshot: set -e; . …/shell_snapshots/… (Codex)",
		match: func(a []string) bool {
			s := shellScript(a)
			return strings.HasPrefix(s, "set -e;") && strings.Contains(s, "shell_snapshots/")
		},
		purpose: "checks that a new shell snapshot loads", inherit: true},
	{pattern: "a shell running env alone: sh -c env (Claude Code's spawn-env probe)",
		match:   func(a []string) bool { return shellScript(a) == "env" },
		purpose: "the spawn-env probe: reads the environment a shell the build starts has, beside the shell snapshot's", inherit: true},
	{pattern: "sandbox-exec …", match: func(a []string) bool { return cmdName(a) == "sandbox-exec" },
		purpose: "runs the tool command in a sandbox (macOS Seatbelt)", tools: true},
	{pattern: "bwrap …", match: func(a []string) bool { return cmdName(a) == "bwrap" },
		purpose: "runs the tool command in a sandbox (bubblewrap)", tools: true},
	{pattern: "codex-linux-sandbox …", match: func(a []string) bool { return cmdName(a) == "codex-linux-sandbox" },
		purpose: "runs the tool command in a sandbox (Codex's Linux sandbox helper, which starts bubblewrap)", tools: true},
	{pattern: "… --codex-run-as-process-setup … (Codex itself, as its process-setup helper)",
		match:   func(a []string) bool { return slices.Contains(a, "--codex-run-as-process-setup") },
		purpose: "sets up the tool command's process, then starts Codex's Linux sandbox helper", tools: true},
	{pattern: "path_helper …", match: func(a []string) bool { return cmdName(a) == "path_helper" },
		purpose: "builds a login shell's PATH from /etc/paths: macOS's /etc/zprofile runs it in every login shell", tools: true},
	{pattern: "id -u", match: func(a []string) bool { return slices.Equal(a, []string{"id", "-u"}) },
		purpose: "reads the user's id: the system's /etc/profile runs it in every login shell", tools: true},
	{pattern: "security find-generic-password …",
		match: func(a []string) bool {
			return cmdName(a) == "security" && len(a) > 1 && a[1] == "find-generic-password"
		},
		purpose: "looks up a stored credential in the macOS Keychain"},
	{pattern: "ps -o lstart= …", match: func(a []string) bool { return cmdName(a) == "ps" && slices.Contains(a, "lstart=") },
		purpose: "reads a process's start time"},
	{pattern: "ps aux, alone or in a shell's pipeline",
		match: func(a []string) bool {
			return cmdName(a) == "ps" && slices.Contains(a, "aux") || strings.HasPrefix(shellScript(a), "ps aux")
		},
		purpose: "lists the running processes, to find an IDE to connect to", inherit: true},
	{pattern: "ioreg … IOPlatformExpertDevice, alone or in a shell",
		match: func(a []string) bool {
			return (cmdName(a) == "ioreg" || isShell(a[0])) && strings.Contains(strings.Join(a, " "), "IOPlatformExpertDevice")
		},
		purpose: "reads the machine's hardware identifiers", inherit: true},
	{pattern: "uname, or a shell's script of uname and command -v (the package-manager probe)",
		match: func(a []string) bool {
			s := shellScript(a)
			return cmdName(a) == "uname" || strings.HasPrefix(s, "uname") || strings.Contains(s, "command -v")
		},
		purpose: "detects the platform and installed package managers", inherit: true},
	{pattern: "getconf LONG_BIT", match: func(a []string) bool { return slices.Equal(a, []string{"getconf", "LONG_BIT"}) },
		purpose: "reads the platform's word size"},
	{pattern: "git … ls-files --error-unmatch … settings.local.json",
		match: func(a []string) bool {
			sub, rest := gitCommand(a)
			return sub == "ls-files" && slices.ContainsFunc(rest, func(r string) bool { return strings.Contains(r, "settings.local.json") })
		},
		purpose: "checks whether the repository tracks the local settings file", inherit: true},
	{pattern: "git … config user.name | user.email",
		match: func(a []string) bool {
			sub, rest := gitCommand(a)
			return sub == "config" && (slices.Contains(rest, "user.name") || slices.Contains(rest, "user.email"))
		},
		purpose: "reads the user's git identity", inherit: true},
	{pattern: "git … status | log | rev-parse | remote | branch | config | diff | show | describe | ls-files | for-each-ref | symbolic-ref | rev-list | merge-base",
		match: func(a []string) bool {
			sub, _ := gitCommand(a)
			return slices.Contains([]string{"status", "log", "rev-parse", "remote", "branch", "config", "diff", "show", "describe",
				"ls-files", "for-each-ref", "symbolic-ref", "rev-list", "merge-base"}, sub)
		},
		purpose: "reads the repository's state for the model's context", inherit: true},
	{pattern: "rg --version", match: func(a []string) bool { return cmdName(a) == "rg" && slices.Contains(a, "--version") },
		purpose: "checks that the bundled ripgrep runs"},
	{pattern: "rg --files … plugins/cache",
		match: func(a []string) bool {
			return cmdName(a) == "rg" && slices.ContainsFunc(a, func(s string) bool { return strings.Contains(s, "plugins/cache") })
		},
		purpose: "looks for plugin caches marked orphaned, to remove them"},
	{pattern: "rg --files …", match: func(a []string) bool { return cmdName(a) == "rg" && slices.Contains(a, "--files") },
		purpose: "lists the repository's files"},
}

// execPurpose is the purpose execPurposes give a normalized command line, and whether the
// programs it starts share it; tool says the command is a tool call's.
func execPurpose(argv []string, tool bool) (string, bool) {
	if len(argv) == 0 {
		return "", false
	}
	for _, r := range execPurposes {
		if (r.tools || !tool) && r.match(argv) {
			return r.purpose, r.inherit
		}
	}
	return "", false
}

// cmdName is a command line's program, by its base name.
func cmdName(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	return filepath.Base(argv[0])
}

// shellScript is the script a shell's command line runs, its first argument that is no option
// (`sh -c -l SCRIPT`, `zsh -lc SCRIPT`); "" for any other command line.
func shellScript(argv []string) string {
	if len(argv) < 2 || !isShell(argv[0]) {
		return ""
	}
	for _, a := range argv[1:] {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// gitCommand is a git command line's subcommand past git's own options (`-c key=value`,
// `-C dir`), and its arguments; "" for any other command line.
func gitCommand(argv []string) (string, []string) {
	if cmdName(argv) != "git" {
		return "", nil
	}
	for i := 1; i < len(argv); i++ {
		switch a := argv[i]; {
		case a == "-c" || a == "-C" || a == "--git-dir" || a == "--work-tree" || a == "--namespace":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a, argv[i+1:]
		}
	}
	return "", nil
}
