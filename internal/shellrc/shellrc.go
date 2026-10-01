// Package shellrc finds the developer's shell startup file and writes the line that
// puts a directory first on PATH in that shell.
package shellrc

import (
	"cmp"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// RC is the developer's shell startup file.
type RC struct {
	Path  string
	Shell string // "zsh", "bash" or "fish"
}

// ShellRC is the startup file of the login shell ($SHELL), or false for a shell terma cannot write for.
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
		// macOS terminals start login shells, which read .bash_profile and not .bashrc.
		if profile := filepath.Join(home, ".bash_profile"); runtime.GOOS == "darwin" && exists(profile) {
			return RC{Path: profile, Shell: shell}, true
		}
		return RC{Path: filepath.Join(home, ".bashrc"), Shell: shell}, true
	case "fish":
		// A file of terma's own, in which fish_add_path moves the entry to the front each run.
		return RC{Path: filepath.Join(FishConfigDir(home), "conf.d", "terma.fish"), Shell: shell}, true
	}
	return RC{}, false
}

// FishConfigDir is $XDG_CONFIG_HOME/fish, else home's .config/fish.
func FishConfigDir(home string) string {
	return filepath.Join(cmp.Or(os.Getenv("XDG_CONFIG_HOME"), filepath.Join(home, ".config")), "fish")
}

// PathLine is the line that puts binDir first on PATH, in the shell's own syntax, with
// a directory under home written as $HOME/….
func (rc RC) PathLine(binDir string) string {
	// Double quotes let $HOME expand; the literal path is escaped so metacharacters stay inert.
	prefix, dir := "", binDir
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, binDir); err == nil && !strings.HasPrefix(rel, "..") {
			prefix, dir = "$HOME/", filepath.ToSlash(rel)
		}
	}
	if rc.Shell == "fish" {
		// fish treats a backtick in double quotes as literal.
		return `fish_add_path --move --prepend "` + prefix + escapeDoubleQuoted(dir, false) + `"`
	}
	return `export PATH="` + prefix + escapeDoubleQuoted(dir, true) + `:$PATH"`
}

// escapeDoubleQuoted escapes what stays active inside a double-quoted string, backslash
// first; fish passes backtick=false.
func escapeDoubleQuoted(s string, backtick bool) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `$`, `\$`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	if backtick {
		s = strings.ReplaceAll(s, "`", "\\`")
	}
	return s
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
