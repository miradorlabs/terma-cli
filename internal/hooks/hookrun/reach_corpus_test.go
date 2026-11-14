package hookrun

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookruntest"
	"github.com/miradorlabs/terma-cli/internal/runs"
)

// corpus is a personal checkout, side, beside Acme's, acme-api, in one folder: side has a
// Go module's tree, 120 files in internal/cli and 45 in internal/hooks/hookrun among them,
// and p, a file naming Acme's checkout.
type corpus struct {
	side, api string
}

func newCorpus(t *testing.T) corpus {
	t.Helper()
	parent, _ := filepath.EvalSymlinks(t.TempDir())
	c := corpus{side: hookruntest.RepoIn(t, parent, "github.com/dawson/side"), api: hookruntest.RepoIn(t, parent, "github.com/acme/acme-api")}
	for i := range 120 {
		hookruntest.WriteFile(t, c.side, fmt.Sprintf("internal/cli/f%03d.go", i), "package cli\n")
	}
	for i := range 45 {
		hookruntest.WriteFile(t, c.side, fmt.Sprintf("internal/hooks/hookrun/h%02d.go", i), "package hookrun\n")
	}
	for _, root := range []string{c.side, c.api} {
		for _, rel := range []string{"internal/a/a.go", "internal/b/b.go", "internal/c/c.go", "tests/test_x.py", "Makefile", "go.mod", "package.json", "NOTES.md"} {
			hookruntest.WriteFile(t, root, rel, "x\n")
		}
	}
	hookruntest.WriteFile(t, c.side, "p", "../acme-api\n")
	return c
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
	}
}

// corpusCrossing are commands that reach Acme's checkout: each of corpusFine's rewritten to,
// and one whose reach cannot be resolved.
func corpusCrossing() map[string]string {
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
	}
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
// reach somewhere the scan cannot resolve, does both.
func TestRoutineAgentCommandsDoNotTurnTheRunMixed(t *testing.T) {
	t.Parallel()
	c := newCorpus(t)
	m := newRoutedMachine(t, acmeAndPersonalRepos(t))
	for _, set := range []struct {
		name     string
		commands map[string]string
		mixed    bool
	}{{"fine", corpusFine(), false}, {"crossing", corpusCrossing(), true}} {
		for name, command := range set.commands {
			t.Run(set.name+"/"+name, func(t *testing.T) {
				t.Parallel()
				paths := ShellPaths(command, c.side)
				if got := c.reachesAcme(paths); got != set.mixed {
					t.Errorf("ShellPaths reaches Acme or somewhere not known: %v, want %v (%d paths: %.400q)", got, set.mixed, len(paths), paths)
				}
				sid := "corpus-" + set.name + "-" + strings.NewReplacer(" ", "-", "/", "-").Replace(name)
				r := startIn(t, m.hook(), sid, c.side)
				env := m.hook()
				env.Cwd = c.side
				env.Classify(context.Background(), r, sessionOf(sid), paths, nil)
				if ledger, placement := m.mixed(sid); ledger != set.mixed || placement != set.mixed {
					t.Errorf("through Classify: ledger Mixed %v, mixed placement %v; want %v", ledger, placement, set.mixed)
				}
				// Doctor tells a crossing from a command it could not read.
				want := runs.Why{}
				switch {
				case name == "cd to where a file says":
					want = runs.Why{Reason: runs.UnknownReach, Where: `cd "$(cat p)" && …`}
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
