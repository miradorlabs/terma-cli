package boundary

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

var update = flag.Bool("update", false, "rewrite testdata/leaks.txt from the current tree")

const module = "github.com/miradorlabs/terma-cli"

// agents are the coding agents terma integrates with, by the name their package takes.
var agents = []string{"claude", "codex", "cursor", "antigravity", "opencode", "omp", "pi", "hermes", "gemini", "dsh"}

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
	"internal/relay/allow.go": "the one safe-key table for every agent: a key that is content anywhere is content",
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

// TestAgentPackagesAreImportedOnlyByTheRegistry holds the import graph: an agent's
// package is imported by the registry and by nothing else, never by another agent, and
// the relay core stays free of every harness package.
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
		if p.ImportPath == module+"/internal/relay" || strings.HasPrefix(p.ImportPath, module+"/internal/relay/claim") {
			for _, imp := range p.Imports {
				if imp == module+"/internal/harness" || strings.HasPrefix(imp, module+"/internal/harness/") {
					t.Errorf("%s imports %s: the relay core learns about agents only through relay.Options", p.ImportPath, imp)
				}
			}
		}
	}
}

// TestAgentMentionsOnlyShrink counts, in every shipped file outside an agent's package,
// the identifiers and strings that name an agent, and holds each package to its count
// in testdata/leaks.txt. A package, not a file: splitting a file is how code gets ready
// to move.
func TestAgentMentionsOnlyShrink(t *testing.T) {
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
	baselinePath := filepath.Join("testdata", "leaks.txt")
	if *update {
		writeBaseline(t, baselinePath, got)
		return
	}
	want := readBaseline(t, baselinePath)
	total, wantTotal := 0, 0
	for file, n := range got {
		total += n
		switch was, ok := want[file]; {
		case !ok:
			t.Errorf("%s names an agent %d times: an agent's code belongs in internal/agents/<name>, and internal/agents/builtin is what lists them", file, n)
		case n > was:
			t.Errorf("%s names agents %d times, up from %d", file, n, was)
		case n < was:
			t.Errorf("%s names agents %d times, down from %d: record the progress with -update", file, n, was)
		}
	}
	for file, was := range want {
		wantTotal += was
		if _, ok := got[file]; !ok {
			t.Errorf("%s no longer names an agent (was %d): record the progress with -update", file, was)
		}
	}
	t.Logf("agent mentions outside agent packages: %d (baseline %d)", total, wantTotal)
}

var agentWord = regexp.MustCompile(`(?i)(^|[^a-z])(claude|codex|cursor|antigravity|opencode|omp|pi|hermes|gemini|dsh)([^a-z]|$)`)

// mentions counts the identifiers and string literals in a file that name an agent.
// Comments are prose and do not count.
func mentions(t *testing.T, path string) int {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
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
			}
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				if s, err := strconv.Unquote(x.Value); err == nil && stringNamesAgent(s) {
					n++
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

func readBaseline(t *testing.T, path string) map[string]int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]int{}
		}
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	out := map[string]int{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		count, file, ok := strings.Cut(line, " ")
		n, err := strconv.Atoi(count)
		if !ok || err != nil {
			t.Fatalf("%s: bad line %q", path, line)
		}
		out[file] = n
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func writeBaseline(t *testing.T, path string, got map[string]int) {
	t.Helper()
	files := make([]string, 0, len(got))
	total := 0
	for file, n := range got {
		files = append(files, file)
		total += n
	}
	sort.Strings(files)
	var b strings.Builder
	fmt.Fprintf(&b, "# Identifiers and strings naming a coding agent, per package, in shipped files outside\n")
	fmt.Fprintf(&b, "# the agents' own packages. This list only shrinks. Total: %d.\n", total)
	for _, file := range files {
		fmt.Fprintf(&b, "%d %s\n", got[file], file)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
