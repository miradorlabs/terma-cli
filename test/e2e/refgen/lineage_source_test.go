package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// gitIn runs git in dir, failing the test where it fails.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// codexTree writes a release of a miniature Codex: a command line, a schema, a variable read.
func codexTree(t *testing.T, dir string, flags []string, settings []string, env []string) {
	t.Helper()
	var fields strings.Builder
	for _, f := range flags {
		fields.WriteString("    #[arg(long = \"" + f + "\")]\n    " + strings.ReplaceAll(f, "-", "_") + ": bool,\n")
	}
	main := "use clap::Parser;\n\n#[derive(Debug, Parser)]\n#[clap(version)]\nstruct MultitoolCli {\n" + fields.String() + "}\n\nfn main() {\n"
	for _, e := range env {
		main += "    let _ = std::env::var(\"" + e + "\");\n"
	}
	main += "}\n"
	props := []string{}
	for _, s := range settings {
		props = append(props, `"`+s+`": {"type": "string"}`)
	}
	schema := `{"type": "object", "properties": {` + strings.Join(props, ", ") + `}}`
	for p, data := range map[string]string{
		"codex-rs/cli/Cargo.toml":          "[package]\nname = \"codex-cli\"\n",
		"codex-rs/cli/src/main.rs":         main,
		"codex-rs/core/config.schema.json": schema,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// The lookup walks the releases back from the oldest build tracked, names the release and the
// commit that introduced each item, where it is defined there and in the newest build, and
// leaves an item it cannot tell without a source.
func TestLineageSourceLookup(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	origin := t.TempDir()
	gitIn(t, origin, "init", "-q", "-b", "main")
	commit := func(msg string) string {
		gitIn(t, origin, "add", "-A")
		gitIn(t, origin, "commit", "-q", "-m", msg)
		return gitIn(t, origin, "rev-parse", "HEAD")
	}
	codexTree(t, origin, []string{"old"}, []string{"model"}, []string{"CODEX_HOME"})
	commit("first")
	gitIn(t, origin, "tag", "rust-v0.1.0")
	codexTree(t, origin, []string{"old"}, []string{"model"}, []string{"CODEX_HOME"})
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("unrelated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit("unrelated")
	codexTree(t, origin, []string{"old", "json"}, []string{"model", "approval"}, []string{"CODEX_HOME", "CODEX_API_KEY"})
	added := commit("add --json, approval, CODEX_API_KEY")
	gitIn(t, origin, "tag", "rust-v0.2.0")
	codexTree(t, origin, []string{"zzz", "old", "json"}, []string{"model", "approval"}, []string{"CODEX_HOME", "CODEX_API_KEY"})
	commit("move things")
	gitIn(t, origin, "tag", "rust-v0.3.0")

	root := t.TempDir()
	gitIn(t, root, "clone", "-q", origin, clonePath(root, "openai/codex"))
	clone, err := ensureClone(root, "openai/codex") // fetches its tags
	if err != nil {
		t.Fatal(err)
	}
	pick, err := lineageSourcers(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	ss, _ := pick("codex").(*sourceSourcer)
	if ss == nil || ss.clone == nil {
		t.Fatal("no sourcer reads the clone")
	}
	ss.lookups, ss.log = true, nil

	snap := func(v string, flags []string, settings []string, env []string) *Snapshot {
		return lineageSnap(v, map[string][]string{"codex": append(flags, "--help", "--version")}, settings, env)
	}
	versions := []string{"0.2.0", "0.3.0"}
	byVersion := map[string]*Snapshot{
		"0.2.0": snap("0.2.0", []string{"--old", "--json", "--ghost"}, []string{"model", "approval"}, []string{"CODEX_HOME", "CODEX_API_KEY"}),
		"0.3.0": snap("0.3.0", []string{"--old", "--json", "--zzz"}, []string{"model", "approval"}, []string{"CODEX_HOME", "CODEX_API_KEY"}),
	}
	dir := t.TempDir()
	if err := writeLineage(dir, "codex", versions, byVersion, ss); err != nil {
		t.Fatal(err)
	}
	l, err := readLineage(dir)
	if err != nil {
		t.Fatal(err)
	}
	json := l.Items["cli.commands.codex.options.--json"]
	if json.Source == nil {
		t.Fatalf("--json has no source: %s", jsonOf(json))
	}
	s := json.Source
	if s.Tag != "rust-v0.2.0" || s.Commit != added || s.File != "codex-rs/cli/src/main.rs" || s.Line != 8 ||
		!strings.Contains(s.Verified, "absent at rust-v0.1.0, present at rust-v0.2.0") ||
		s.Permalink != "https://github.com/openai/codex/blob/rust-v0.2.0/codex-rs/cli/src/main.rs#L8" {
		t.Errorf("--json source = %s", jsonOf(s))
	}
	if json.Ranges[0].Since != "0.2.0" || !json.SinceBounded {
		t.Errorf("--json = %s", jsonOf(json))
	}
	// The newest build defines it two lines down.
	if s.Current == nil || s.Current.Tag != "rust-v0.3.0" || s.Current.Line != 10 {
		t.Errorf("--json current = %s", jsonOf(s.Current))
	}
	// In the first release: its commit is the first.
	old := l.Items["cli.commands.codex.options.--old"]
	if old.Source == nil || old.Source.Tag != "rust-v0.1.0" || !strings.Contains(old.Source.Verified, "the first release") {
		t.Errorf("--old = %s", jsonOf(old))
	}
	for id, tag := range map[string]string{"config.options.approval": "rust-v0.2.0", "config.env_vars.CODEX_API_KEY": "rust-v0.2.0",
		"config.options.model": "rust-v0.1.0"} {
		if it := l.Items[id]; it.Source == nil || it.Source.Tag != tag || it.Source.Commit == "" || it.Source.File == "" {
			t.Errorf("%s = %s", id, jsonOf(it))
		}
	}
	if it := l.Items["config.env_vars.CODEX_API_KEY"]; it.Source != nil && it.Source.Commit != added {
		t.Errorf("CODEX_API_KEY commit = %s, want %s", it.Source.Commit, added)
	}
	// Not in the source at all: not guessed.
	if ghost := l.Items["cli.commands.codex.options.--ghost"]; ghost.Source != nil {
		t.Errorf("--ghost = %s", jsonOf(ghost))
	}
	// Introduced within the window: no lookup.
	if zzz := l.Items["cli.commands.codex.options.--zzz"]; zzz.Source != nil {
		t.Errorf("--zzz = %s", jsonOf(zzz))
	}

	// A later render with no clone keeps every source; one with the clone refreshes current
	// from the file it was in.
	if err := writeLineage(dir, "codex", versions, byVersion, nil); err != nil {
		t.Fatal(err)
	}
	again, _ := readLineage(dir)
	if !reflect.DeepEqual(jsonOf(again), jsonOf(l)) {
		t.Errorf("a render without the clone changed the lineage:\n%s\nvs\n%s", jsonOf(again), jsonOf(l))
	}

	// A newer release moves --json down: a render that can read it moves current there, from
	// the file it was in, and keeps the rest.
	codexTree(t, origin, []string{"aaa", "zzz", "old", "json"}, []string{"model", "approval"}, []string{"CODEX_HOME", "CODEX_API_KEY"})
	commit("more")
	gitIn(t, origin, "tag", "rust-v0.4.0")
	if _, err := ensureClone(root, "openai/codex"); err != nil {
		t.Fatal(err)
	}
	byVersion["0.4.0"] = snap("0.4.0", []string{"--old", "--json", "--zzz", "--aaa"}, []string{"model", "approval"}, []string{"CODEX_HOME", "CODEX_API_KEY"})
	pick, _ = lineageSourcers(root, nil, false)
	if err := writeLineage(dir, "codex", append(versions, "0.4.0"), byVersion, pick("codex")); err != nil {
		t.Fatal(err)
	}
	moved, _ := readLineage(dir)
	cur := moved.Items["cli.commands.codex.options.--json"].Source.Current
	if cur == nil || cur.Tag != "rust-v0.4.0" || cur.Line != 12 || cur.File != "codex-rs/cli/src/main.rs" {
		t.Errorf("--json current = %s", jsonOf(cur))
	}
	if got := moved.Items["cli.commands.codex.options.--json"].Source.Commit; got != added {
		t.Errorf("--json commit = %s", got)
	}
	_ = clone
}

// An option a new command inherits is added by the commit that adds the command, which
// touches no line that names the option: the lookup finds it on the release's line of commits.
func TestLineageSourceInheritedOption(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	origin := t.TempDir()
	gitIn(t, origin, "init", "-q", "-b", "main")
	write := func(main string) {
		for p, data := range map[string]string{
			"codex-rs/cli/Cargo.toml":    "[package]\nname = \"codex-cli\"\n",
			"codex-rs/cli/src/shared.rs": "use clap::Args;\n\n#[derive(Args, Debug)]\npub struct Shared {\n    #[arg(long)]\n    pub model: Option<String>,\n}\n",
			"codex-rs/cli/src/main.rs":   main,
		} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(origin, p)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(origin, p), []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		gitIn(t, origin, "add", "-A")
		gitIn(t, origin, "commit", "-q", "-m", "change")
	}
	root := "use clap::Parser;\nuse crate::shared::Shared;\n\n#[derive(Debug, Parser)]\nstruct MultitoolCli {\n    #[clap(flatten)]\n    shared: Shared,\n%s}\n%s"
	execCmd := "\n#[derive(Debug, clap::Subcommand)]\nenum Sub {\n    Exec(ExecCli),\n}\n\n#[derive(Debug, Parser)]\nstruct ExecCli {\n    #[clap(flatten)]\n    shared: Shared,\n}\n"
	sub := "    #[clap(subcommand)]\n    sub: Option<Sub>,\n"
	write(fmt.Sprintf(root, "", ""))
	gitIn(t, origin, "tag", "rust-v0.1.0")
	write(fmt.Sprintf(root, "", "// a comment\n"))
	write(fmt.Sprintf(root, sub, execCmd))
	added := gitIn(t, origin, "rev-parse", "HEAD")
	write(fmt.Sprintf(root, sub, execCmd+"// more\n"))
	gitIn(t, origin, "tag", "rust-v0.2.0")

	clones := t.TempDir()
	gitIn(t, clones, "clone", "-q", origin, clonePath(clones, "openai/codex"))
	pick, err := lineageSourcers(clones, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	ss := pick("codex").(*sourceSourcer)
	ss.lookups, ss.log = true, nil
	snap := lineageSnap("0.2.0", map[string][]string{"codex": {"--model", "--help"}, "codex exec": {"--model", "--help"}}, nil, nil)
	dir := t.TempDir()
	if err := writeLineage(dir, "codex", []string{"0.2.0"}, map[string]*Snapshot{"0.2.0": snap}, ss); err != nil {
		t.Fatal(err)
	}
	l, _ := readLineage(dir)
	for _, id := range []string{"cli.commands.codex exec.options.--model", "cli.commands.codex exec.options.--help", "cli.commands.codex exec"} {
		it := l.Items[id]
		if it.Source == nil || it.Source.Tag != "rust-v0.2.0" || it.Source.Commit != added {
			t.Errorf("%s = %s, want commit %s", id, jsonOf(it), added)
		}
	}
	if it := l.Items["cli.commands.codex.options.--model"]; it.Source == nil || it.Source.Tag != "rust-v0.1.0" {
		t.Errorf("--model = %s", jsonOf(it))
	}
}
