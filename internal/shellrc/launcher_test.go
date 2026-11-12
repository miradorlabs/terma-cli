package shellrc

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// A launcher is written once into every startup file the shell reads, replaced in place
// when written again, and removed leaving the developer's own lines exactly as they were.
func TestLauncherInstallsOnceAndRemovesCleanly(t *testing.T) {
	home := rcSandbox(t, "bash")
	mine := "export EDITOR=vim\nalias ll='ls -l'\n"
	for _, name := range []string{".bashrc", ".bash_profile"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(mine), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{filepath.Join(home, ".bashrc"), filepath.Join(home, ".bash_profile")}
	changed, err := InstallLauncher("agentx")
	if err != nil || !slices.Equal(changed, want) {
		t.Fatalf("install changed %v, %v", changed, err)
	}
	for _, path := range want {
		data, _ := os.ReadFile(path)
		if !strings.HasPrefix(string(data), mine+"\n# >>> terma launch agentx >>>\n") || strings.Count(string(data), "terma launch agentx --") != 1 {
			t.Fatalf("%s after install:\n%s", path, data)
		}
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Errorf("%s became %v", path, info.Mode().Perm())
		}
	}
	if !LauncherInstalled("agentx") || LauncherInstalled("other") {
		t.Fatal("LauncherInstalled does not read the block it wrote")
	}
	// A login shell reads only its login file: without the block there, it is not installed.
	login, _ := os.ReadFile(want[1])
	if err := os.WriteFile(want[1], []byte(mine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if LauncherInstalled("agentx") {
		t.Fatal("LauncherInstalled with the block in .bashrc alone")
	}
	if err := os.WriteFile(want[1], login, 0o600); err != nil {
		t.Fatal(err)
	}
	if again, err := InstallLauncher("agentx"); err != nil || len(again) > 0 {
		t.Fatalf("a second install changed %v, %v", again, err)
	}
	// The developer's lines after the block stay after it.
	rc := filepath.Join(home, ".bashrc")
	data, _ := os.ReadFile(rc)
	if err := os.WriteFile(rc, append(data, "export LATER=1\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveLauncher("agentx")
	if err != nil || !slices.Equal(removed, want) {
		t.Fatalf("remove changed %v, %v", removed, err)
	}
	if data, _ := os.ReadFile(rc); string(data) != mine+"export LATER=1\n" {
		t.Fatalf(".bashrc after removal:\n%q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(home, ".bash_profile")); string(data) != mine {
		t.Fatalf(".bash_profile after removal:\n%q", data)
	}
	if again, err := RemoveLauncher("agentx"); err != nil || len(again) > 0 {
		t.Fatalf("a second removal changed %v, %v", again, err)
	}
}

// A startup file that is a link into a dotfiles checkout is written through, keeping the link.
func TestLauncherWritesThroughALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no shell startup files on Windows")
	}
	home := rcSandbox(t, "zsh")
	dotfiles := t.TempDir()
	target := filepath.Join(dotfiles, "zshrc")
	if err := os.WriteFile(target, []byte("setopt autocd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".zshrc")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallLauncher("agentx"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v %v", info, err)
	}
	if data, _ := os.ReadFile(target); !strings.Contains(string(data), "terma launch agentx --") {
		t.Fatalf("the link's target lacks the launcher:\n%s", data)
	}
}

// A begin marker whose end marker the developer deleted is not a block: what they wrote
// below it survives a second install and a removal, which touch only the whole block.
func TestLauncherLeavesWhatFollowsAnOrphanedMarker(t *testing.T) {
	home := rcSandbox(t, "zsh")
	rc := filepath.Join(home, ".zshrc")
	begin, _ := launcherMarkers("agentx")
	mine := "export A=1\n" + begin + "\n# the end marker was deleted\nexport KEEP=1\nalias gs='git status'\n"
	if err := os.WriteFile(rc, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := InstallLauncher("agentx"); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(rc)
	if !strings.HasPrefix(string(data), mine) || strings.Count(string(data), "terma launch agentx --") != 1 {
		t.Fatalf("after two installs:\n%s", data)
	}
	if _, err := RemoveLauncher("agentx"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(rc); string(data) != mine {
		t.Fatalf("after removal:\n%q\nwant:\n%q", data, mine)
	}
	// A marker inside a longer line is not a marker.
	if _, _, found := cutLauncher("echo '"+begin+"'\n# <<< terma launch agentx <<< trailing\n", "agentx"); found {
		t.Fatal("found a block where no line is a marker")
	}

	// An end marker left above with no begin hides nothing: the block below it is still
	// the one a second install replaces and a removal takes out.
	_, end := launcherMarkers("agentx")
	stray := end + "\nexport B=2\n"
	if err := os.WriteFile(rc, []byte(stray), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := InstallLauncher("agentx"); err != nil {
			t.Fatal(err)
		}
	}
	data, _ = os.ReadFile(rc)
	if !strings.HasPrefix(string(data), stray) || strings.Count(string(data), "terma launch agentx --") != 1 || !LauncherInstalled("agentx") {
		t.Fatalf("after three installs below a stray end marker:\n%s", data)
	}
	if _, err := RemoveLauncher("agentx"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(rc); string(data) != stray {
		t.Fatalf("after removal:\n%q\nwant:\n%q", data, stray)
	}
}

// A shell terma writes nothing for, an unusable name and the kill switch are each said, never guessed around.
func TestLauncherRefusesWhatItCannotWrite(t *testing.T) {
	rcSandbox(t, "nu")
	if _, err := InstallLauncher("agentx"); err != ErrNoShell {
		t.Fatalf("an unknown shell: %v", err)
	}
	if changed, err := RemoveLauncher("agentx"); err != nil || len(changed) > 0 {
		t.Fatalf("removal under an unknown shell: %v, %v", changed, err)
	}
	rcSandbox(t, "zsh")
	for _, name := range []string{"", "two words", "a;b", "$(x)", "Upper"} {
		if _, err := InstallLauncher(name); err == nil {
			t.Errorf("wrote a function named %q", name)
		}
	}
	if LaunchersOff() {
		t.Fatal("the kill switch is on by default")
	}
	t.Setenv("TERMA_SHELL_LAUNCHERS", "0")
	if !LaunchersOff() {
		t.Fatal("TERMA_SHELL_LAUNCHERS=0 was not read")
	}
}

// The block parses in every shell that may source the file, defines the function where no
// alias holds the name, and leaves an alias of that name, and the shell's start, alone.
func TestLauncherBlockIsSafeToSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX shells on Windows")
	}
	block := RC{Shell: "zsh"}.LauncherBlock("agentx")
	for _, shell := range []string{"sh", "dash", "bash", "zsh"} {
		path, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		t.Run(shell, func(t *testing.T) {
			dir := t.TempDir()
			// A stand-in for the real command, and for terma, on PATH.
			for name, body := range map[string]string{
				"agentx": "#!/bin/sh\necho \"real $*\"\n",
				"terma":  "#!/bin/sh\necho \"terma $*\"\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			run := func(script, pathEnv string) string {
				t.Helper()
				cmd := exec.Command(path, "-c", script)
				cmd.Env = []string{"PATH=" + pathEnv, "HOME=" + dir}
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%s: %v\n%s", shell, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			both := dir + ":/usr/bin:/bin"
			if got := run(block+"agentx -p 'two words'", both); got != "terma launch agentx -- -p two words" {
				t.Errorf("with terma: %q", got)
			}
			// Without terma on PATH the command runs as it always did.
			bare := t.TempDir()
			if err := os.WriteFile(filepath.Join(bare, "agentx"), []byte("#!/bin/sh\necho \"real $*\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if got := run(block+"agentx -p x", bare+":/usr/bin:/bin"); got != "real -p x" {
				t.Errorf("without terma: %q", got)
			}
			// An alias of the same name is the developer's: the block defines nothing over it.
			// Sourced as a startup file is, a command at a time, so the alias is known when
			// the shell reads the block.
			aliased := "alias agentx='echo aliased'\n" + block + "agentx now\n"
			if shell == "bash" {
				aliased = "shopt -s expand_aliases\n" + aliased
			}
			rc := filepath.Join(dir, "rc")
			if err := os.WriteFile(rc, []byte(aliased), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := run(". "+rc, both); got != "aliased now" {
				t.Errorf("beside an alias: %q", got)
			}
			// So is a function of the same name, and sourcing the file twice changes nothing.
			own := "agentx() { echo \"mine $*\"; }\n" + block + block + "agentx now\n"
			if err := os.WriteFile(rc, []byte(own), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := run(". "+rc, both); got != "mine now" {
				t.Errorf("beside the developer's own function: %q", got)
			}
			if got := run("set -eu\n"+block+block+"agentx -p x", both); got != "terma launch agentx -- -p x" {
				t.Errorf("sourced twice under set -eu: %q", got)
			}
			// With the command not installed yet, the shell still starts.
			if got := run("set -eu\n"+block+"echo started", "/usr/bin:/bin"); got != "started" {
				t.Errorf("with no such command: %q", got)
			}
		})
	}
}

// fish reads its own syntax, from a file of terma's own, and defines the function only where
// the developer has none of that name.
func TestLauncherBlockForFish(t *testing.T) {
	home := rcSandbox(t, "fish")
	changed, err := InstallLauncher("agentx")
	want := filepath.Join(home, ".config", "fish", "conf.d", "terma.fish")
	if err != nil || !slices.Equal(changed, []string{want}) {
		t.Fatalf("install changed %v, %v", changed, err)
	}
	data, _ := os.ReadFile(want)
	for _, line := range []string{"if not functions -q agentx\n", "    function agentx\n", "terma launch agentx -- $argv\n", "command agentx $argv\n"} {
		if !strings.Contains(string(data), line) {
			t.Errorf("fish block lacks %q:\n%s", line, data)
		}
	}
	if fish, err := exec.LookPath("fish"); err == nil {
		if out, err := exec.Command(fish, "--no-execute", want).CombinedOutput(); err != nil {
			t.Fatalf("fish does not parse the block: %v\n%s", err, out)
		}
	}
}
