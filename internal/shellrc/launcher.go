package shellrc

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// A launcher is a shell function terma defines in the developer's startup files for a
// command that takes its exporter from the environment alone: typing the command runs
// `terma launch <command>`, which starts the real one with what terma adds to its
// environment, and the real one as before once terma is gone. Like a hook entry it only
// calls terma: the endpoint and the relay's token stay out of files that often live in a
// dotfiles repository.

// ErrNoShell means $SHELL is not one terma writes startup files for.
var ErrNoShell = errors.New("terma writes shell functions for zsh, bash and fish only")

// launcherName is a command a launcher can be named for: one word every shell reads as is.
var launcherName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// LaunchersOff reports the kill switch `TERMA_SHELL_LAUNCHERS=0`, which keeps terma's
// functions out of the shell startup files.
func LaunchersOff() bool {
	return os.Getenv("TERMA_SHELL_LAUNCHERS") == "0"
}

func launcherMarkers(name string) (begin, end string) {
	return "# >>> terma launch " + name + " >>>", "# <<< terma launch " + name + " <<<"
}

// LauncherBlock is the marked lines that define name's launcher, in the shell's own syntax.
func (rc RC) LauncherBlock(name string) string {
	begin, end := launcherMarkers(name)
	note := "# Runs " + name + " through terma, which adds its telemetry settings; without terma it runs as before."
	var body string
	if rc.Shell == "fish" {
		// A function or alias the developer already gave the name stays theirs: conf.d runs
		// before their functions directory autoloads one and before config.fish, so the
		// function is defined only where `functions -q`, which autoloads, finds none.
		body = "if not functions -q " + name + "\n" +
			"    function " + name + "\n" +
			"        if command -q terma\n" +
			"            terma launch " + name + " -- $argv\n" +
			"        else\n" +
			"            command " + name + " $argv\n" +
			"        end\n" +
			"    end\n" +
			"end"
	} else {
		// An alias or a function the developer already gave the name stays theirs: the
		// function is defined only where the name is a program on PATH, or nothing yet. It
		// is read through eval because a shell expands an alias as it reads a function's
		// name, which beside one would be a syntax error on every start.
		body = `case "$(command -v ` + name + ` 2>/dev/null)" in
/* | "") eval '` + name + `() {
  if command -v terma >/dev/null 2>&1; then
    terma launch ` + name + ` -- "$@"
  else
    command ` + name + ` "$@"
  fi
}' ;;
esac`
	}
	return begin + "\n" + note + "\n" + body + "\n" + end + "\n"
}

// InstallLauncher writes name's launcher into the login shell's startup files, replacing an
// earlier one in place, and returns the files it changed.
func InstallLauncher(name string) ([]string, error) {
	return editLaunchers(name, true)
}

// RemoveLauncher takes name's launcher out of the login shell's startup files and returns
// the files it changed; the rest of each file stays as it was.
func RemoveLauncher(name string) ([]string, error) {
	changed, err := editLaunchers(name, false)
	if errors.Is(err, ErrNoShell) {
		return nil, nil
	}
	return changed, err
}

// LauncherInstalled reports whether every startup file setup writes defines name's launcher:
// bash reads .bashrc in an interactive shell and its login file in a login shell (a macOS
// terminal), and either may start the command.
func LauncherInstalled(name string) bool {
	rc, ok := ShellRC()
	if !ok || len(rc.Paths) == 0 {
		return false
	}
	for _, path := range rc.Paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		if _, _, found := cutLauncher(string(data), name); !found {
			return false
		}
	}
	return true
}

func editLaunchers(name string, install bool) ([]string, error) {
	if !launcherName.MatchString(name) {
		return nil, errors.New("shellrc: " + name + " cannot name a shell function")
	}
	rc, ok := ShellRC()
	if !ok {
		return nil, ErrNoShell
	}
	var changed []string
	for _, path := range rc.Paths {
		did, err := editLauncher(path, name, rc.LauncherBlock(name), install)
		if err != nil {
			return changed, err
		}
		if did {
			changed = append(changed, path)
		}
	}
	return changed, nil
}

func editLauncher(path, name, block string, install bool) (bool, error) {
	// Often a dotfiles link: written through, as the agents' settings are.
	writePath, _, err := config.ResolveWritePath(path)
	if err != nil {
		return false, err
	}
	mode := fs.FileMode(0o644)
	data, err := os.ReadFile(writePath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !install {
			return false, nil
		}
	case err != nil:
		return false, err
	default:
		if info, err := os.Stat(writePath); err == nil {
			mode = info.Mode().Perm()
		}
	}
	before := string(data)
	head, tail, found := cutLauncher(before, name)
	var after string
	switch {
	case install && found:
		after = head + block + tail
	case install:
		after = before
		if after != "" && !strings.HasSuffix(after, "\n") {
			after += "\n"
		}
		if after != "" {
			after += "\n"
		}
		after += block
	case found:
		// The blank line InstallLauncher put before the block goes with it.
		if strings.HasSuffix(head, "\n\n") {
			head = strings.TrimSuffix(head, "\n")
		}
		after = head + tail
	default:
		return false, nil
	}
	if after == before {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(writePath), 0o755); err != nil {
		return false, err
	}
	return true, config.WriteFileAtomic(writePath, []byte(after), mode)
}

// cutLauncher splits text around name's marked block: what precedes its first line and
// what follows its last. The block is the first end marker's line that has a begin marker's
// line above it, with the nearest such line: a marker the developer left without its pair
// names nothing, so what they wrote around it is never taken for terma's, and a stray one
// never hides the block.
func cutLauncher(text, name string) (head, tail string, found bool) {
	begin, end := launcherMarkers(name)
	start := -1
	for at := 0; at < len(text); {
		line, next := text[at:], len(text)
		if nl := strings.IndexByte(line, '\n'); nl >= 0 {
			line, next = line[:nl], at+nl+1
		}
		switch line {
		case begin:
			start = at
		case end:
			if start >= 0 {
				return text[:start], text[next:], true
			}
		}
		at = next
	}
	return text, "", false
}
