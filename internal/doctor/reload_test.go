package doctor

import (
	"testing"
)

// `source` is not POSIX: a dash or BusyBox ash user is told `.`.
func TestReloadCommandMatchesTheShell(t *testing.T) {
	for shell, want := range map[string]string{
		"/bin/zsh": "source ~/.zshrc", "/usr/bin/fish": "source ~/.zshrc", "/bin/dash": ". ~/.zshrc", "": ". ~/.zshrc",
	} {
		t.Setenv("SHELL", shell)
		if got := ReloadCommand("~/.zshrc"); got != want {
			t.Errorf("SHELL=%q: ReloadCommand = %q, want %q", shell, got, want)
		}
	}
}
