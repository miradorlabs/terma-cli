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

// agents are every registered agent's name: its package and the word that names it.
var agents = builtin.Agents("").Names()

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

var mayNameAgents = map[string]string{
	module + "/internal/agents/builtin": "registers every agent",
	module + "/internal/contract":       "byte snapshots, named by agent",
	module + "/internal/boundary":       "this test",
}

// global are the files that name agents by rule rather than by leak, each with the rule.
var global = map[string]string{
	"internal/ui/style/style.go": "the environment variables coding agents set, terma's or not, to tell a model from a person",
}

type pkg struct {
	ImportPath                string
	Dir                       string
	GoFiles, TestGoFiles      []string
	XTestGoFiles              []string
	Imports, Deps             []string
	TestImports, XTestImports []string
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
		// go test caches by what this process reads, and go list reads in its own: read
		// the files here or an import-graph change is answered from the cache.
		if _, err := os.ReadDir(p.Dir); err != nil {
			t.Fatal(err)
		}
		for _, name := range slices.Concat(p.GoFiles, p.TestGoFiles, p.XTestGoFiles) {
			if _, err := os.ReadFile(filepath.Join(p.Dir, name)); err != nil {
				t.Fatal(err)
			}
		}
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

// bans are imports a package and everything below it never make; a package ending in $ is that package alone.
var bans = []struct {
	pkg    string
	banned []string
	why    string
}{
	{"internal/relay", []string{"internal/harness", "internal/agents"}, "the relay learns about agents only through its options"},
	{"internal/relay$", []string{"internal/routing"}, "the engine takes a resolved policy; the daemon resolves it"},
	{"internal/relay", []string{"internal/account/api", "internal/policy"}, "the relay reaches the network only through what the command line injects"},
	{"internal/policy", []string{"internal/cli", "internal/agents", "internal/relay", "internal/hooks"}, "the collection policy is fetched and kept for whoever asks, knowing none of them"},
	{"internal/doctor", []string{"internal/account", "internal/spool", "internal/relay/daemon"}, "doctor reaches credentials, keys, the network, the spool and the relay only through its probes"},
	{"internal/delivery", []string{"internal/cli", "internal/agents", "internal/relay", "internal/doctor", "internal/account/auth"}, "delivery sends what the spool holds with each project's key, asking the agents only through its router"},
	{"internal/setup", []string{"internal/account", "internal/spool", "internal/cli", "internal/relay"}, "setup signs in, prompts and reaches the relay only through its steps"},
	{"internal/refresh", []string{"internal/account", "internal/spool", "internal/cli"}, "a refresh rewrites what terma wrote, from disk alone"},
	{"internal/globalmode", []string{"internal/account", "internal/spool", "internal/cli", "internal/relay"}, "global mode writes this machine's files and nothing else"},
	{"internal/repohooks", []string{"internal/account", "internal/spool", "internal/cli", "internal/relay", "internal/agents", "internal/hooks", "internal/doctor", "internal/globalmode"}, "the hook installer writes a repository's own hook files, from the policy and the filesystem alone"},
	{"internal/hooks/hookrun", []string{"internal/hooks/hookmgr"}, "running a hook and planning hook files are separate halves"},
	{"internal/hooks/hookmgr", []string{"internal/hooks/hookrun"}, "running a hook and planning hook files are separate halves"},
	{"internal/hooks/hookrun", []string{"internal/agents", "internal/cli", "internal/account", "internal/harness"}, "the hook runtime knows no agent, command, account or exporter; agents build on it"},
	{"internal/hooks/hookmgr", []string{"internal/agents", "internal/cli", "internal/account", "internal/harness"}, "hook files are planned for agents, never by naming them"},
	{"internal/hooks/hookruntest", []string{"internal/agents", "internal/cli", "internal/account", "internal/harness"}, "the hook runtime's test kit knows no agent"},
	{"internal/hooks/dispatch", []string{"internal/cli", "internal/account", "internal/harness", "internal/relay/daemon"}, "a hook reaches the relay and the network only through what the command line injects"},
	{"internal/agents", []string{"internal/hooks/dispatch"}, "the registry is what dispatch reads, never the other way round"},
	{"internal/agents", []string{"internal/routing", "internal/relay/claim", "internal/account/keystore", "internal/account/auth", "internal/account/api"}, "an agent is handed the project's route, consent and keys; it never reads them itself"},
	{"internal/account", []string{"internal/agents", "internal/hooks", "internal/relay", "internal/cli", "internal/ui"}, "the account packages talk to the platform and nothing else"},
	{"internal/ui", []string{"internal/account", "internal/agents", "internal/hooks", "internal/relay", "internal/cli", "internal/config"}, "terminal output depends on nothing of terma's"},
}

// onlyImportedBy are packages only one entry point imports outside tests.
var onlyImportedBy = map[string]string{
	"internal/cli":            "cmd/terma",
	"internal/agents/builtin": "cmd/terma",
}

func within(path, pkg string) bool {
	return path == module+"/"+pkg || strings.HasPrefix(path, module+"/"+pkg+"/")
}

// TestAgentPackagesAreImportedOnlyByTheRegistry holds the import graph: only the registry
// imports an agent's package, no agent imports another, and no ban is broken.
func TestAgentPackagesAreImportedOnlyByTheRegistry(t *testing.T) {
	t.Parallel()
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
		// A test may build on the registry, but only the snapshots are about one agent.
		for _, imp := range append(p.TestImports, p.XTestImports...) {
			if other, ok := agentPackage(imp); ok && (!isAgent || other != self) && p.ImportPath != module+"/internal/contract" {
				t.Errorf("%s's tests import %s: a test reaches an agent through internal/agents/builtin", p.ImportPath, imp)
			}
		}
		for _, imp := range p.Imports {
			if imp == module+"/internal/agents/agentstest" {
				t.Errorf("%s imports %s: the made-up agent is for tests only", p.ImportPath, imp)
			}
			for pkg, by := range onlyImportedBy {
				if imp == module+"/"+pkg && p.ImportPath != module+"/"+by {
					t.Errorf("%s imports %s: only %s does", p.ImportPath, imp, by)
				}
			}
		}
		for _, ban := range bans {
			if pkg, exact := strings.CutSuffix(ban.pkg, "$"); exact && p.ImportPath != module+"/"+pkg || !exact && !within(p.ImportPath, ban.pkg) {
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

// Every package directly under internal/agents is a registered agent, the registry, or
// shared code: an unregistered agent would be neither wired nor guarded.
func TestEveryAgentPackageIsRegistered(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(filepath.Join(repoRoot(t), "internal", "agents"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "builtin" || e.Name() == "internal" || e.Name() == "agentstest" {
			continue
		}
		if !slices.Contains(agents, e.Name()) {
			t.Errorf("internal/agents/%s is not registered in internal/agents/builtin", e.Name())
		}
	}
}

// TestNothingElseNamesAnAgent finds identifiers and strings naming an agent in shipped
// files outside an agent's package; there must be none.
func TestNothingElseNamesAnAgent(t *testing.T) {
	t.Parallel()
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

// mentions counts the identifiers, string literals and comments in a file that name an
// agent.
func mentions(t *testing.T, path string) int {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution|parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, g := range f.Comments {
		for _, c := range g.List {
			if strings.HasPrefix(c.Text, "//go:") || !stringNamesAgent(c.Text) {
				continue
			}
			n++
			if *list {
				t.Logf("%s: %s", fset.Position(c.Pos()), c.Text)
			}
		}
	}
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

// identNamesAgent reports whether a word of a Go identifier (captureCodexFunding →
// capture, codex, funding) is an agent's name.
func identNamesAgent(name string) bool {
	lower := strings.ToLower(name)
	if strings.Contains(lower, "opencode") || strings.Contains(lower, "antigravity") {
		return true
	}
	ws := words(name)
	for i, w := range ws {
		if w == "cursor" {
			// Usually a read position (nextCursor); the editor leads a name (cursorTool).
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

// cursorAgent is the editor in a string, never a read position ("next_cursor").
var cursorAgent = regexp.MustCompile(`^cursor$|^cursor[-_.]|^CURSOR_|^\.cursor|(^|[^A-Za-z])Cursor([^A-Za-z]|$)`)

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

// plural reports whether runes[i] is the s pluralising the acronym before it (PIDs).
func plural(runes []rune, i int) bool {
	return runes[i] == 's' && (i+1 == len(runes) || !unicode.IsLower(runes[i+1]))
}
