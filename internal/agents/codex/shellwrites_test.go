package codex

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestShellWrites(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	if err := os.Mkdir(filepath.Join(cwd, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A Windows path goes into a POSIX command quoted, with forward slashes.
	abs := "'" + filepath.ToSlash(cwd) + "'"
	in := func(p ...string) []string {
		out := make([]string, len(p))
		for i, x := range p {
			out[i] = filepath.Join(cwd, x)
		}
		return out
	}
	for _, tc := range []struct {
		name, command string
		want          []string
	}{
		{"reads only", "go test ./... 2>&1 | tail -5 && rg foo src", nil},
		{"heredoc into a file", "cat > src/x.go <<'EOF'\npackage x\n// a > b\nEOF\ngo vet ./...", in("src/x.go")},
		{"indented heredoc", "cat <<-EOF >> notes.md\n\tline > other\n\tEOF", in("notes.md")},
		{"append", "echo hi >> notes.md", in("notes.md")},
		{"redirect in quotes", `echo "a > b" 'c > d'`, nil},
		{"devices and directories", "go build ./... >/dev/null; echo x > src", nil},
		{"stderr and descriptors", "make 2> err.log 2>&1 >&2", nil},
		{"stdout and stderr", "make &> build.log", in("build.log")},
		{"tee", "go test ./... | tee -a out.log", in("out.log")},
		{"GNU sed -i", "sed -i 's/a/b/' a.go b.go", in("a.go", "b.go")},
		{"BSD sed -i", "sed -i '' -e 's/a/b/' -e 's/c/d/' a.go", in("a.go")},
		{"sed backup suffix", "sed -E -i.bak 's/a/b/' a.go", in("a.go")},
		{"sed long script options", "sed -i --expression='s/a/b/' a.go; sed --in-place --file=fix.sed b.go", in("a.go", "b.go")},
		{"sed attached script", "sed -i -e's/a/b/' a.go; sed -i -ffix.sed b.go", in("a.go", "b.go")},
		{"sed reads", "sed -n '1,20p' a.go", nil},
		{"perl -pi", "perl -pi -e 's/a/b/' a.go", in("a.go")},
		{"perl module is not -i", "perl -Mstrict -e 'print 1' a.go", nil},
		{"gofmt -w", "gofmt -s -w a.go src", in("a.go")},
		{"gofmt lists", "gofmt -l a.go", nil},
		{"biome --write", "biome check --write src/a.ts", in("src/a.ts")},
		{"prettier --write", "prettier --write a.ts", in("a.ts")},
		{"prettier option values", "prettier --write --config .prettierrc --single-quote a.ts", in("a.ts")},
		{"cp destination", "cp -p tmpl.go src/b.go", in("src/b.go")},
		{"mv both ends", "mv old.go new.go", in("old.go", "new.go")},
		{"cp into a directory", "cp a.go b.go src/", in("src/a.go", "src/b.go")},
		{"mv into a directory", "mv a.go src", in("a.go", "src/a.go")},
		{"git mv both ends", "git mv -f old.go new.go", in("old.go", "new.go")},
		{"git mv into a directory made first", "mkdir -p docs/notes && git mv src/p4.txt docs/notes/p4.txt && git commit -m move", in("src/p4.txt", "docs/notes/p4.txt")},
		{"into a directory made first", "mkdir -p pairs && printf 'seven\\n' > pairs/o7.txt && git add -- pairs/o7.txt", in("pairs/o7.txt")},
		{"git reads", "git add a.go && git commit -m mv", nil},
		{"cp into a directory made first", "mkdir out && cp input.txt out", in("out/input.txt")},
		{"into a directory made with a mode", "mkdir -m 0755 newdir && printf x > newdir/f.txt", in("newdir/f.txt")},
		{"subshell cd ends with it", "(cd src && gofmt -w a.go); gofmt -w b.go", in("src/a.go", "b.go")},
		{"named >& target", "make >& build.log", in("build.log")},
		{"function body runs nothing", "f() { cd src; echo x > g.txt; }; echo y > h.txt", in("h.txt")},
		{"command substitution's cd ends with it", "v=$(cd src && pwd); echo x > i.txt", in("i.txt")},
		{"does not parse", "echo x > j.txt; if then", nil},
		{"into a missing directory", "echo x > nowhere/f.txt", nil},
		{"cd first", "cd src && cat > c.go <<'EOF'\npackage src\nEOF", in("src/c.go")},
		{"cd absolute", "cd " + abs + "/src; echo x > d.txt", in("src/d.txt")},
		{"cd the shell decides", `cd "$REPO" && echo x > e.txt`, nil},
		{"absolute after an unknown cd", `cd "$REPO" && echo x > ` + abs + "/e.txt", in("e.txt")},
		// POSIX shell: an unquoted \ escapes the next character, so unquoted C:\x\y names C:xy.
		{"unquoted backslashes", `echo x > a\ b.txt`, nil},
		{"cd in a pipeline", "cd src | cat; echo x > n.txt", in("n.txt")},
		{"cd in a branch", "if test -d src; then cd src; echo x > k.txt; fi; echo y > l.txt", in("src/k.txt")},
		{"expanded targets", "echo x > $OUT; sed -i 's/a/b/' *.go; echo y > ~/f", nil},
		{"assignment prefix", "GOFLAGS=-mod=mod gofmt -w a.go", in("a.go")},
		{"comment", "echo hi # > nope.txt", nil},
		// A patch body is not shell: its redirect writes nothing.
		{"patch heredoc", "apply_patch <<'PATCH'\n*** Begin Patch\n*** Add File: x.sh\n+echo a > out.txt\n*** End Patch\nPATCH", in("x.sh")},
		{"patch in a command that does not parse", "apply_patch <<'PATCH'\n*** Begin Patch\n*** Add File: z.txt\n+z\n*** End Patch\nPATCH\nif then", in("z.txt")},
		{"wrapped patch", "command apply_patch <<'PATCH'\n*** Begin Patch\n*** Add File: w.txt\n+w\n*** End Patch\nPATCH", in("w.txt")},
		{"patch by path", "/usr/local/bin/apply_patch <<'PATCH'\n*** Begin Patch\n*** Add File: v.txt\n+v\n*** End Patch\nPATCH", in("v.txt")},
		{"command lookup runs nothing", "command -v cp ./agent.txt ./f.txt; command -pV sed -i s/a/b/ a.go", nil},
		{"env -C moves the writer", "env -C sub sed -i s/x/y/ f.txt; env --chdir=sub gofmt -w g.go", nil},
		{"wrapped writer", "env LC_ALL=C sed -i 's/a/b/' a.go; command -p gofmt -w b.go", in("a.go", "b.go")},
		{"patch argument", "apply_patch '*** Begin Patch\n*** Add File: new/y.sh\n+y\n*** End Patch'", in("new/y.sh")},
		{"patch after a cd", "cd src && apply_patch <<'PATCH'\n*** Begin Patch\n*** Update File: a.go\n@@\n-a\n+b\n*** End Patch\nPATCH", in("src/a.go")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := shellWrites(tc.command, cwd); !slices.Equal(got, tc.want) {
				t.Fatalf("shellWrites(%q)\n got %v\nwant %v", tc.command, got, tc.want)
			}
		})
	}
}

// committed counts the writes a commit in the command takes in; a write after it is not its.
func TestShellWritesBeforeEachCommit(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	for _, tc := range []struct {
		name, command string
		want          []int
	}{
		{"no commit", "printf x > a.txt", nil},
		{"write then commit", "printf x > a.txt && git add a.txt && git commit -m a", []int{1}},
		{"commit then write", "git commit -m human && printf agent > f.txt", []int{0}},
		{"two commits", "git commit -m human && printf agent > h && git add h && git commit -m agent", []int{0, 1}},
		{"git options before commit", "printf x > a.txt; git -C . -c user.name=x commit -m a", []int{1}},
		{"wrapped commit", "printf x > a.txt && git add a.txt && command git commit -m a", []int{1}},
		{"commit under env -u", "printf x > a.txt && env -u GIT_DIR git commit -m a", []int{1}},
		{"commit under env", "printf x > a.txt && env GIT_AUTHOR_NAME=x git commit -m a", []int{1}},
		{"a conflict resolved and the pick continued", "printf ok > f.txt && git add f.txt && GIT_EDITOR=true git cherry-pick --continue", []int{1}},
		{"commit lookup is no commit", "printf x > a.txt && command -v git commit", nil},
		{"commit message is not a subcommand", "git log --grep commit && printf x > a.txt", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := shellWrites(tc.command, cwd); !slices.Equal(got, tc.want) {
				t.Fatalf("commits = %v, want %v", got, tc.want)
			}
		})
	}
}
