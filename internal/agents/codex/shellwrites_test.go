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
		// Codex's own patch heredoc'd into a shell call is applyPatchPaths' to read.
		{"patch body", "apply_patch <<'PATCH'\n*** Begin Patch\n*** Add File: x.sh\n+echo a > out.txt\n*** End Patch\nPATCH", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shellWrites(tc.command, cwd); !slices.Equal(got, tc.want) {
				t.Fatalf("shellWrites(%q)\n got %v\nwant %v", tc.command, got, tc.want)
			}
		})
	}
}
