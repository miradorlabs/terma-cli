package cmd

import "testing"

// `source` is not POSIX: a dash or BusyBox ash user is told `.`.
func TestReloadCommandMatchesTheShell(t *testing.T) {
	for shell, want := range map[string]string{
		"/bin/zsh": "source ~/.zshrc", "/usr/bin/fish": "source ~/.zshrc", "/bin/dash": ". ~/.zshrc", "": ". ~/.zshrc",
	} {
		t.Setenv("SHELL", shell)
		if got := reloadCommand("~/.zshrc"); got != want {
			t.Errorf("SHELL=%q: reloadCommand = %q, want %q", shell, got, want)
		}
	}
}
