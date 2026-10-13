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
		body = "function " + name + "\n" +
			"    if command -q terma\n" +
			"        terma launch " + name + " -- $argv\n" +
			"    else\n" +
			"        command " + name + " $argv\n" +
			"    end\n" +
			"end"
	} else {
		// A shell expands an alias as it reads a function's name, so one of the same name
		// would make the definition a syntax error on every start: the alias stays, and the
		// function is read only when there is none.
		body = "alias " + name + " >/dev/null 2>&1 || eval '" + name + `() {
  if command -v terma >/dev/null 2>&1; then
    terma launch ` + name + ` -- "$@"
  else
    command ` + name + ` "$@"
  fi
}'`
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

// LauncherInstalled reports whether the startup file a new shell reads defines name's launcher.
func LauncherInstalled(name string) bool {
	rc, ok := ShellRC()
	if !ok || len(rc.Paths) == 0 {
		return false
	}
	data, err := os.ReadFile(rc.Paths[0])
	if err != nil {
		return false
	}
	_, _, found := cutLauncher(string(data), name)
	return found
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
// what follows its last.
func cutLauncher(text, name string) (head, tail string, found bool) {
	begin, end := launcherMarkers(name)
	i := strings.Index(text, begin+"\n")
	if i < 0 || i > 0 && text[i-1] != '\n' {
		return text, "", false
	}
	j := strings.Index(text[i:], "\n"+end)
	if j < 0 {
		return text, "", false
	}
	rest := text[i+j+len("\n"+end):]
	rest = strings.TrimPrefix(rest, "\n")
	return text[:i], rest, true
}
