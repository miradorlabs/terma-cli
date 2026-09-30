package shim

import (
	"cmp"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// Earlier builds put the shim directory on PATH through one marked block at the end of
// the developer's shell startup file. RC finds that file, and Remove takes the block out
// again, leaving every other byte. PathLine is the one line that puts a directory first
// on PATH in the file's own shell, which doctor quotes for terma's own directory.

const (
	rcBegin = "# >>> terma per-repo routing >>>"
	rcEnd   = "# <<< terma per-repo routing <<<"
)

// RC is the developer's shell startup file.
type RC struct {
	Path  string
	Shell string // "zsh", "bash" or "fish"
}

// ShellRC is the startup file of the developer's login shell ($SHELL), or false for a
// shell terma does not know how to write for — the caller then prints the line instead.
func ShellRC() (RC, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return RC{}, false
	}
	switch shell := filepath.Base(os.Getenv("SHELL")); shell {
	case "zsh":
		dir := cmp.Or(os.Getenv("ZDOTDIR"), home)
		return RC{Path: filepath.Join(dir, ".zshrc"), Shell: shell}, true
	case "bash":
		// macOS terminals start login shells, which read .bash_profile and not .bashrc;
		// a .bash_profile that exists is therefore the file that is actually read there.
		if profile := filepath.Join(home, ".bash_profile"); runtime.GOOS == "darwin" && exists(profile) {
			return RC{Path: profile, Shell: shell}, true
		}
		return RC{Path: filepath.Join(home, ".bashrc"), Shell: shell}, true
	case "fish":
		// A file of terma's own: conf.d is read after config.fish's PATH edits only in
		// name order, so the block is the whole file and fish_add_path moves the entry
		// to the front whenever it runs.
		return RC{Path: filepath.Join(FishConfigDir(home), "conf.d", "terma.fish"), Shell: shell}, true
	}
	return RC{}, false
}

// FishConfigDir is fish's configuration directory for the given home directory:
// $XDG_CONFIG_HOME/fish, else ~/.config/fish.
func FishConfigDir(home string) string {
	return filepath.Join(cmp.Or(os.Getenv("XDG_CONFIG_HOME"), filepath.Join(home, ".config")), "fish")
}

// PathLine is the line that puts binDir first on PATH, in the shell's own syntax. A
// directory under the home directory is written as $HOME/… so the file survives being
// copied to another machine.
func (rc RC) PathLine(binDir string) string {
	// The directory is written inside double quotes so a bare $HOME prefix still
	// expands, but the literal path is escaped so a directory containing shell
	// metacharacters ($ " ` \) can neither break out of the quotes nor trigger
	// expansion or command substitution when the startup file is sourced. A benign
	// path escapes to itself, so the emitted line is unchanged for normal installs.
	prefix, dir := "", binDir
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, binDir); err == nil && !strings.HasPrefix(rel, "..") {
			prefix, dir = "$HOME/", filepath.ToSlash(rel)
		}
	}
	if rc.Shell == "fish" {
		// fish does not perform command substitution inside double quotes, so a
		// backtick there is already literal and must not be backslash-escaped.
		return `fish_add_path --move --prepend "` + prefix + escapeDoubleQuoted(dir, false) + `"`
	}
	return `export PATH="` + prefix + escapeDoubleQuoted(dir, true) + `:$PATH"`
}

// escapeDoubleQuoted escapes the characters that stay active inside a double-quoted
// shell string. Backslash is escaped first so the escapes it introduces are not
// re-escaped. POSIX/zsh treat backtick as command substitution inside double quotes;
// fish does not, so it passes backtick=false.
func escapeDoubleQuoted(s string, backtick bool) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `$`, `\$`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	if backtick {
		s = strings.ReplaceAll(s, "`", "\\`")
	}
	return s
}

// Remove takes terma's block out of the file, leaving the rest as it was. A fish file
// that held nothing else is deleted. It reports whether anything was removed.
func (rc RC) Remove() (bool, error) {
	data, err := os.ReadFile(rc.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	before, after, found := splitBlock(string(data))
	if !found {
		return false, nil
	}
	// The blank line install put ahead of the block goes with it.
	rest := strings.TrimSuffix(before, "\n") + after
	if strings.TrimSpace(rest) == "" && rc.Shell == "fish" {
		return true, os.Remove(rc.Path)
	}
	return true, rc.write(rest)
}

func (rc RC) write(content string) error {
	if err := os.MkdirAll(filepath.Dir(rc.Path), 0o755); err != nil {
		return err
	}
	mode := fs.FileMode(0o644)
	if fi, err := os.Stat(rc.Path); err == nil {
		mode = fi.Mode().Perm()
	}
	// A startup file is often a symlink into a dotfiles repository. Write through it:
	// renaming a temporary file over the link would replace it with a regular file.
	target := rc.Path
	if resolved, err := filepath.EvalSymlinks(rc.Path); err == nil {
		target = resolved
	}
	return config.WriteFileAtomic(target, []byte(content), mode)
}

// splitBlock cuts content around terma's block: what precedes the begin marker, and what
// follows the end marker's line. found is false when either marker is missing, in which
// case the file is treated as not carrying the block at all.
func splitBlock(content string) (before, after string, found bool) {
	start := strings.Index(content, rcBegin)
	if start < 0 {
		return content, "", false
	}
	end := strings.Index(content[start:], rcEnd)
	if end < 0 {
		return content, "", false
	}
	end += start + len(rcEnd)
	if nl := strings.IndexByte(content[end:], '\n'); nl >= 0 {
		end += nl + 1
	} else {
		end = len(content)
	}
	return content[:start], content[end:], true
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
