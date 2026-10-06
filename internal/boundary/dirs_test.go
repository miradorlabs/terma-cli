package boundary

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// resolvers are the config package's functions that read terma's directories from the
// environment, and the only files that may call each; everything else is handed the result.
var resolvers = map[string][]string{
	"Dir": {"internal/cli/root.go", "scripts/benchhooks/main.go"},
	// The bench installs a repository's commit hooks, whose record is state, so it resolves
	// both directories exactly as the command line does.
	"StateDir": {"internal/cli/root.go", "scripts/benchhooks/main.go"},
	// The relay service is named for whether its state directory is the default one.
	"DefaultStateDir": {"internal/relay/daemon/service.go"},
}

// TestOnlyTheEntryPointsResolveTheDirs holds config.Dir and config.StateDir to the command
// line's entry (and the bench script): below it each directory is a value passed in, never
// read again.
func TestOnlyTheEntryPointsResolveTheDirs(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	fset := token.NewFileSet()
	callers := map[string][]string{}
	for _, p := range listPackages(t) {
		for _, name := range p.GoFiles {
			path := filepath.Join(p.Dir, name)
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			local := ""
			for _, imp := range f.Imports {
				if v, _ := strconv.Unquote(imp.Path.Value); v == module+"/internal/config" {
					local = "config"
					if imp.Name != nil {
						local = imp.Name.Name
					}
				}
			}
			if local == "" {
				continue
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && resolvers[sel.Sel.Name] != nil {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == local {
						rel, _ := filepath.Rel(root, path)
						callers[sel.Sel.Name] = append(callers[sel.Sel.Name], filepath.ToSlash(rel))
					}
				}
				return true
			})
		}
	}
	for name, want := range resolvers {
		got := slices.Compact(slices.Sorted(slices.Values(callers[name])))
		if !slices.Equal(got, want) {
			t.Errorf("config.%s is read in %v, want only %v: pass the directory down instead", name, got, want)
		}
	}
}
