package boundary

import (
	"encoding/json"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
)

var list = flag.Bool("mentions", false, "log every mention of an agent the test finds")

const module = "github.com/miradorlabs/terma-cli"

// agents are the coding agents terma integrates with, by the name their package takes.
// agents are every registered agent's name: the package it lives in, and the word that
// names it.
var agents = builtin.Agents().Names()

// agentPackage reports the agent an import path belongs to: internal/agents/<name> and
// anything below it.
func agentPackage(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, module+"/internal/agents/")
	if !ok {
		return "", false
	}
	name, _, _ := strings.Cut(rest, "/")
	for _, a := range agents {
		if name == a {
			return a, true
		}
	}
	return "", false
}

// mayNameAgents are the packages whose job is to name agents: the one that registers
// them, and these tests' own fixtures.
var mayNameAgents = map[string]string{
	module + "/internal/agents/builtin": "registers every agent",
	module + "/internal/contract":       "byte snapshots, named by agent",
	module + "/internal/boundary":       "this test",
}

// global are the files that name agents by rule rather than by leak, each with the
// rule. Nothing else belongs here.
var global = map[string]string{
	"internal/style/style.go": "the environment variables coding agents set, terma's or not, to tell a model from a person",
	"internal/api/ai.go":      "the gateway's pagination cursor",
}

type pkg struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Imports    []string
}

func listPackages(t *testing.T) []pkg {
	t.Helper()
	cmd := exec.Command("go", "list", "-json", "./...")
	cmd.Dir = repoRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var pkgs []pkg
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var p pkg
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// bans are imports a package and everything below it never make, each with why.
var bans = []struct {
	pkg    string
	banned []string
	why    string
}{
	{"internal/relay", []string{"internal/harness", "internal/agents"}, "the relay learns about agents only through its options"},
	{"internal/doctor", []string{"internal/api", "internal/spool"}, "doctor reaches the network and the spool only through its probes"},
}

// within reports whether path is pkg or below it.
func within(path, pkg string) bool {
	return path == module+"/"+pkg || strings.HasPrefix(path, module+"/"+pkg+"/")
}

// TestAgentPackagesAreImportedOnlyByTheRegistry holds the import graph: an agent's
// package is imported by the registry and by nothing else, never by another agent, and
// no package makes an import its bans forbid.
func TestAgentPackagesAreImportedOnlyByTheRegistry(t *testing.T) {
	for _, p := range listPackages(t) {
		self, isAgent := agentPackage(p.ImportPath)
		for _, imp := range p.Imports {
			other, ok := agentPackage(imp)
			switch {
			case !ok:
			case isAgent && other == self:
			case isAgent:
				t.Errorf("%s imports %s: one agent never imports another; share through internal/agents/internal", p.ImportPath, imp)
			case p.ImportPath != module+"/internal/agents/builtin":
				t.Errorf("%s imports %s: only internal/agents/builtin imports an agent's package", p.ImportPath, imp)
			}
		}
		for _, ban := range bans {
			if !within(p.ImportPath, ban.pkg) {
				continue
			}
			for _, imp := range p.Imports {
				if slices.ContainsFunc(ban.banned, func(b string) bool { return within(imp, b) }) {
					t.Errorf("%s imports %s: %s", p.ImportPath, imp, ban.why)
				}
			}
		}
	}
}

// Every package directly under internal/agents is an agent the build registers, the
// registry itself, or code agents share: an agent left out of builtin would be neither
// wired nor guarded.
func TestEveryAgentPackageIsRegistered(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(repoRoot(t), "internal", "agents"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "builtin" || e.Name() == "internal" {
			continue
		}
		if !slices.Contains(agents, e.Name()) {
			t.Errorf("internal/agents/%s is not registered in internal/agents/builtin", e.Name())
		}
	}
}

// TestNothingElseNamesAnAgent finds, in every shipped file outside an agent's package,
// the identifiers and strings that name an agent. There are none: what the core needs of
// an agent it asks through internal/agents.
func TestNothingElseNamesAnAgent(t *testing.T) {
	root := repoRoot(t)
	got := map[string]int{}
	for _, p := range listPackages(t) {
		if _, ok := agentPackage(p.ImportPath); ok {
			continue
		}
		if strings.HasPrefix(p.ImportPath, module+"/internal/agents/internal/") {
			continue // shared by a few agents, and invisible to everything else
		}
		if _, ok := mayNameAgents[p.ImportPath]; ok {
			continue
		}
		for _, name := range p.GoFiles {
			path := filepath.Join(p.Dir, name)
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if _, ok := global[rel]; ok {
				continue
			}
			if n := mentions(t, path); n > 0 {
				got[filepath.ToSlash(filepath.Dir(rel))] += n
			}
		}
	}
	for dir, n := range got {
		t.Errorf("%s names an agent %d times: an agent's code belongs in internal/agents/<name>, and internal/agents/builtin is what lists them (-mentions lists each)", dir, n)
	}
}

var agentWord = regexp.MustCompile(`(?i)(^|[^a-z])(` + strings.Join(agents, "|") + `)([^a-z]|$)`)

// mentions counts the identifiers and string literals in a file that name an agent.
// Comments are prose and do not count.
func mentions(t *testing.T, path string) int {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.ImportSpec:
			return false // an import is held by the import-graph test
		case *ast.Ident:
			if identNamesAgent(x.Name) {
				n++
				if *list {
					t.Logf("%s: %s", fset.Position(x.Pos()), x.Name)
				}
			}
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				if s, err := strconv.Unquote(x.Value); err == nil && stringNamesAgent(s) {
					n++
					if *list {
						t.Logf("%s: %q", fset.Position(x.Pos()), s)
					}
				}
			}
		}
		return true
	})
	return n
}

// identNamesAgent splits a Go identifier into its words (captureCodexFunding → capture,
// codex, funding) and reports whether one is an agent's name.
func identNamesAgent(name string) bool {
	lower := strings.ToLower(name)
	if strings.Contains(lower, "opencode") || strings.Contains(lower, "antigravity") {
		return true
	}
	ws := words(name)
	for i, w := range ws {
		if w == "cursor" {
			// A read position (fundingCursor, nextCursor) far more often than the editor:
			// the editor leads a longer name (cursorTool, CursorSubagentStop).
			if i == 0 && len(ws) > 1 {
				return true
			}
			continue
		}
		if slices.Contains(agents, w) {
			return true
		}
	}
	return false
}

// cursorAgent is the editor in a string: its name, its directory, its hook events and
// its environment, never a read position ("funding cursor", "next_cursor").
var cursorAgent = regexp.MustCompile(`^cursor$|^cursor[-_.]|^CURSOR_|^\.cursor|(^|[^A-Za-z])Cursor([^A-Za-z]|$)`)

// stringNamesAgent reports whether a string literal names an agent.
func stringNamesAgent(s string) bool {
	for _, m := range agentWord.FindAllStringSubmatch(s, -1) {
		if !strings.EqualFold(m[2], "cursor") {
			return true
		}
	}
	return cursorAgent.MatchString(s)
}

func words(name string) []string {
	var out []string
	var cur []rune
	runes := []rune(name)
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	for i, r := range runes {
		switch {
		case r == '_' || unicode.IsDigit(r):
			flush()
		case unicode.IsUpper(r) && i > 0 && (unicode.IsLower(runes[i-1]) || i+1 < len(runes) && unicode.IsLower(runes[i+1]) && !plural(runes, i+1)):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}

// plural reports whether runes[i] is the s that pluralises the acronym before it
// (PIDs, IDs), which stays with the acronym instead of starting a word.
func plural(runes []rune, i int) bool {
	return runes[i] == 's' && (i+1 == len(runes) || !unicode.IsLower(runes[i+1]))
}
