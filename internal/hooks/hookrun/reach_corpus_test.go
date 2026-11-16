package hookrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/runs"
)

// corpus is a personal checkout, side, beside Acme's, acme-api, in one folder, the home
// folder: side has a Go module's tree, 120 files in internal/cli and 45 in
// internal/hooks/hookrun among them, and p, a file naming Acme's checkout; each has
// programs, x and scripts/dump.sh.
type corpus struct {
	home, side, api string
}

func newCorpus(t testing.TB) corpus {
	t.Helper()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	c := corpus{home: parent, side: hookruntest.RepoIn(t, parent, "github.com/dawson/side"), api: hookruntest.RepoIn(t, parent, "github.com/acme/acme-api")}
	for i := range 120 {
		hookruntest.WriteFile(t, c.side, fmt.Sprintf("internal/cli/f%03d.go", i), "package cli\n")
	}
	for i := range 45 {
		hookruntest.WriteFile(t, c.side, fmt.Sprintf("internal/hooks/hookrun/h%02d.go", i), "package hookrun\n")
	}
	for _, root := range []string{c.side, c.api} {
		for _, rel := range []string{"internal/a/a.go", "internal/b/b.go", "internal/c/c.go", "tests/test_x.py", "Makefile", "go.mod", "package.json", "NOTES.md", "x", "scripts/dump.sh"} {
			hookruntest.WriteFile(t, root, rel, "x\n")
		}
	}
	hookruntest.WriteFile(t, c.side, "p", "../acme-api\n")
	return c
}

// paths is ShellPaths from side, with the corpus's folder for the home folder.
func (c corpus) paths(command string) []string {
	return shellPaths(command, c.side, func(k string) (string, bool) {
		if k == "HOME" {
			return c.home, true
		}
		return os.LookupEnv(k)
	})
}

// applyPatch is a patch sent through the shell, as an agent sends one, of about size bytes:
// hunks updating files, each line of code naming words with slashes that are no paths.
func applyPatch(files []string, size int) string {
	var b strings.Builder
	b.WriteString("apply_patch <<'EOF'\n*** Begin Patch\n*** Add File: internal/newpkg/new.go\n+package newpkg\n")
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, "*** Update File: %s\n@@ func f%d() {\n-\treturn a/b // see docs/old-%d/page and pkg/v%d/x\n+\treturn a / b // see https://example.com/new/%d and cmd/tool%d/main.go\n",
			files[i%len(files)], i, i, i, i, i)
	}
	b.WriteString("*** End Patch\nEOF")
	return b.String()
}

// prBody is a long pull request description in a here-document, naming repository-relative
// paths, some there and some not.
func prBody() string {
	var b strings.Builder
	b.WriteString("## Summary\n\n")
	for i := range 60 {
		fmt.Fprintf(&b, "- internal/cli/f%03d.go: handle the case in cmd/terma/%d, see docs/design/%d.md and the old a/b split\n", i, i, i)
	}
	return b.String()
}

// corpusFine are commands agents run in a personal checkout that reach nothing of Acme's.
func corpusFine() map[string]string {
	var patchFiles, named []string
	for i := range 30 {
		patchFiles = append(patchFiles, fmt.Sprintf("internal/cli/f%03d.go", i*3), fmt.Sprintf("internal/hooks/hookrun/h%02d.go", i))
	}
	for i := range 40 {
		named = append(named, fmt.Sprintf("internal/cli/f%03d.go", i))
	}
	return map[string]string{
		"go test ./...":                 "go test ./...",
		"go test ./internal/...":        "go test ./internal/...",
		"make check":                    "make check",
		"ls over 120 files":             "ls internal/cli/*.go",
		"ls over 45 files":              "ls internal/hooks/hookrun/*.go",
		"gofmt -l .":                    "gofmt -l .",
		"rg TODO internal":              "rg TODO internal",
		"git status and diff":           "git status && git diff --stat",
		"git commit with a heredoc":     "git add -A && git commit -m \"$(cat <<'EOF'\nfix(hooks): read a/b and c/d as data\n\nThe scanner no longer counts words in internal/x.\nEOF\n)\"",
		"gh pr create with a long body": "gh pr create --title \"fix: reach\" --body \"$(cat <<'EOF'\n" + prBody() + "EOF\n)\"",
		"a 40 KB apply_patch":           applyPatch(patchFiles, 40<<10),
		"a for loop over packages":      "for pkg in a b c; do go test ./internal/$pkg/...; done",
		"a linter under GOPATH":         "$(go env GOPATH)/bin/golangci-lint run",
		"npm test":                      "npm test",
		"pnpm install":                  "pnpm install",
		"pytest":                        "python3 -m pytest tests/",
		"find and xargs":                "find . -name '*.go' | xargs wc -l",
		"cd to the top level and make":  "cd \"$(git rev-parse --show-toplevel)\" && make",
		"git add of 40 files":           "git add " + strings.Join(named, " "),
		"echo of 50 bare words":         "echo " + strings.Repeat("word ", 50),
		"rg TODO with DIR unset":        "rg TODO $DIR",
		"a for loop over a glob":        "for f in *.go; do gofmt -l $f; done",
		"git commit -m a heredoc":       "git commit -m \"$(cat <<'EOF'\nfix(hooks): read internal/cli and a/b\nEOF\n)\"",
		"PATH with GOPATH's bin":        "export PATH=$PATH:$(go env GOPATH)/bin",
		"a temporary folder":            "tmp=$(mktemp -d)",
		"a message naming no path":      `git commit -m "fix ../foo"`,
		"ls of the folder above":        "ls ..",
		"git -c":                        "git -c core.pager=cat log",
	}
}

// corpusCrossing are commands that reach Acme's checkout: each of corpusFine's rewritten to,
// others run there, and those whose reach cannot be resolved (corpusUnknown).
func corpusCrossing(c corpus) map[string]string {
	abs := filepath.ToSlash(c.api)
	return map[string]string{
		"go test ./...":                 "go test ../acme-api/...",
		"go test ./internal/...":        "go test ../acme-api/internal/...",
		"make check":                    "make -C ../acme-api check",
		"ls over files":                 "ls ../acme-api/internal/*/*.go",
		"gofmt -l":                      "gofmt -l ../acme-api",
		"rg TODO":                       "rg TODO ../acme-api/internal",
		"git status and diff":           "git -C ../acme-api status && git diff --stat",
		"git commit with a heredoc":     "cd ../acme-api && git add -A && git commit -m \"$(cat <<'EOF'\nfix: x\nEOF\n)\"",
		"gh pr create with a long body": "cd ../acme-api && gh pr create --body \"$(cat <<'EOF'\n" + prBody() + "EOF\n)\"",
		"a 40 KB apply_patch":           applyPatch([]string{"internal/cli/f001.go", "../acme-api/NOTES.md"}, 40<<10),
		"a for loop over packages":      "for pkg in a b c; do go test ../acme-api/internal/$pkg/...; done",
		"a for loop over checkouts":     "for d in ../side ../acme-api; do make -C $d; done",
		"a linter under GOPATH":         "$(go env GOPATH)/bin/golangci-lint run ../acme-api/...",
		"npm test":                      "npm test --prefix ../acme-api",
		"pnpm install":                  "pnpm -C ../acme-api install",
		"pytest":                        "python3 -m pytest ../acme-api/tests/",
		"find and xargs":                "find ../acme-api -name '*.go' | xargs wc -l",
		"cd to the top level and on":    "cd \"$(git rev-parse --show-toplevel)/../acme-api\" && make",
		"cd to where a file says":       "cd \"$(cat p)\" && cat NOTES.md",
		"a script there":                "../acme-api/scripts/dump.sh",
		"a script there under nohup":    "nohup ../acme-api/scripts/dump.sh",
		"a script there under exec":     "exec ../acme-api/scripts/dump.sh",
		"a program there piped to":      "git status | ../acme-api/x",
		"a program there after a cd":    "cd .. && acme-api/x",
		"bash -c":                       `bash -c "cat ../acme-api/NOTES.md"`,
		"sh -c with a cd":               `sh -c 'cd ../acme-api && ls'`,
		"python3 -c":                    `python3 -c "open('../acme-api/NOTES.md')"`,
		"node -e":                       `node -e "require('fs').readFileSync('../acme-api/NOTES.md')"`,
		"git -C where a file says":      `git -C "$(cat p)" log`,
		"make -C where a file says":     "make -C $(cat p)",
		"braces naming checkouts":       "ls {../acme-api,.}",
		"braces in a path":              "cat ../acme-{api,x}/NOTES.md",
		"a here-document piped to sh":   "cat <<EOF | sh\ncd ../acme-api\nEOF",
		// Under a wrapper, the script's path relative or absolute.
		"timeout, bash -c":          "timeout 60 bash -c 'cd ../acme-api && go test ./...'",
		"timeout, bash -c absolute": "timeout 60 bash -c 'cd " + abs + " && go test ./...'",
		"env, bash -c":              "env FOO=1 bash -c 'cat ../acme-api/NOTES.md'",
		"sudo, bash -c absolute":    "sudo bash -c 'cat " + abs + "/NOTES.md'",
		"nice, bash -c":             "nice -n 10 bash -c 'cat ../acme-api/NOTES.md'",
		"xargs, sh -c":              "ls | xargs sh -c 'cat ../acme-api/NOTES.md'",
		"uv run python -c":          `uv run python -c "open('../acme-api/NOTES.md')"`,
		"python3.12 -c absolute":    `python3.12 -c "open('` + abs + `/NOTES.md')"`,
		"wrappers nested":           "sudo timeout 5 env A=1 bash -c 'cat ../acme-api/NOTES.md'",
		// A command that does not parse runs up to its error.
		"cut short after a cd":  "cd ..\ncat acme-api/NOTES.md\necho \"oops",
		"a zsh line after a cd": "cd ..\nfor f (*.go) print $f\ncat acme-api/NOTES.md",
		// A cd back to where the command has been goes there again.
		"cd back and on":         "cd .. && cd side && cd .. && cd acme-api && cat NOTES.md",
		"cd as a subshell did":   "(cd .. && ls); cd .. && cat acme-api/NOTES.md",
		"pushd as before a popd": "pushd .. && ls && popd && pushd .. && cat acme-api/NOTES.md",
		// A recursive reader of a folder above the checkouts.
		"rg of the folder above":   "rg -n TODO ..",
		"grep -r of home":          "grep -rn secret ~/",
		"find of the folder above": `find .. -exec cat {} \;`,
		"rg after a cd":            "cd .. && rg TODO",
	}
}

// underTimeout is command run as timeout 60 bash -c's script.
func underTimeout(command string) string {
	return "timeout 60 bash -c '" + strings.ReplaceAll(command, "'", `'\''`) + "'"
}

// corpusUnknown are corpusCrossing's commands whose reach cannot be resolved, with the
// first words the ledger names each by.
var corpusUnknown = map[string]string{
	"cd to where a file says":   `cd "$(cat p)" && cat …`,
	"git -C where a file says":  `git -C "$(cat p)" log`,
	"make -C where a file says": "make -C $(cat p)",
}

// reachesAcme reports whether paths name Acme's checkout or somewhere not known.
func (c corpus) reachesAcme(paths []string) bool {
	return slices.ContainsFunc(paths, func(p string) bool {
		return isUnknownReach(p) || p == c.api || strings.HasPrefix(p, c.api+string(filepath.Separator))
	})
}

// Commands agents really run in a personal checkout beside Acme's, none of them touching
// Acme's, never turn the run Mixed: neither the scan finds Acme's checkout or a reach not
// known, nor does Classify mark the run. Each rewritten to reach Acme's checkout, or to
// reach somewhere the scan cannot resolve, does both, and so does each of those run as
// timeout 60 bash -c's script.
func TestRoutineAgentCommandsDoNotTurnTheRunMixed(t *testing.T) {
	t.Parallel()
	c := newCorpus(t)
	m := newRoutedMachine(t, acmeAndPersonalRepos(t))
	crossing, wrappedCrossing := corpusCrossing(c), map[string]string{}
	for name, command := range crossing {
		wrappedCrossing[name] = underTimeout(command)
	}
	for _, set := range []struct {
		name     string
		commands map[string]string
		mixed    bool
	}{{"fine", corpusFine(), false}, {"crossing", crossing, true}, {"wrapped", wrappedCrossing, true}} {
		for name, command := range set.commands {
			t.Run(set.name+"/"+name, func(t *testing.T) {
				t.Parallel()
				paths := c.paths(command)
				if got := c.reachesAcme(paths); got != set.mixed {
					t.Errorf("ShellPaths reaches Acme or somewhere not known: %v, want %v (%d paths: %.400q)", got, set.mixed, len(paths), paths)
				}
				sid := "corpus-" + set.name + "-" + strings.Map(func(r rune) rune {
					if r < 'a' || r > 'z' {
						return '-'
					}
					return r
				}, strings.ToLower(name))
				r := startIn(t, m.hook(), sid, c.side)
				env := m.hook()
				env.Cwd = c.side
				env.Classify(context.Background(), r, sessionOf(sid), paths, nil)
				if ledger, placement := m.mixed(sid); ledger != set.mixed || placement != set.mixed {
					t.Errorf("through Classify: ledger Mixed %v, mixed placement %v; want %v", ledger, placement, set.mixed)
				}
				// Doctor tells a crossing from a command it could not read.
				want := runs.Why{}
				switch where, unknown := corpusUnknown[name]; {
				case set.mixed && unknown && set.name == "wrapped":
					want = runs.Why{Reason: runs.UnknownReach, Where: firstWords(command)}
				case set.mixed && unknown:
					want = runs.Why{Reason: runs.UnknownReach, Where: where}
				case set.mixed:
					want = runs.Why{Reason: runs.Crossed, Where: "github.com/acme/acme-api"}
				}
				if why := m.why(sid); why != want {
					t.Errorf("the ledger says the run is Mixed for %+v, want %+v", why, want)
				}
			})
		}
	}
}
