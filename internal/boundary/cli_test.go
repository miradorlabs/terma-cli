package boundary

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// cli is the command tree's package directory.
const cli = "internal/cli"

// TestTheCLIKeepsNoState holds the command tree to one package and its app: it has no
// subdirectories, and no package-level variable
// of the CLI's is assigned, incremented or has its address taken anywhere in the
// package, tests included. A lookup table or an error sentinel is a variable Go cannot
// make a constant; one that changes is state, and state lives in the app.
func TestTheCLIKeepsNoState(t *testing.T) {
	dir := filepath.Join(repoRoot(t), cli)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	vars := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("%s/%s: the command line is one package; what a command needs beneath it is a package of its own under internal", cli, e.Name())
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
		if strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		for _, d := range f.Decls {
			if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.VAR {
				for _, s := range g.Specs {
					for _, n := range s.(*ast.ValueSpec).Names {
						if n.Name != "_" {
							vars[n.Name] = true
						}
					}
				}
			}
		}
	}
	mutated := map[string][]string{}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			local := locals(fn)
			mark := func(e ast.Expr) {
				if name := root(e); vars[name] && !local[name] {
					mutated[name] = append(mutated[name], fset.Position(e.Pos()).String())
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.AssignStmt:
					if x.Tok != token.DEFINE {
						for _, l := range x.Lhs {
							mark(l)
						}
					}
				case *ast.IncDecStmt:
					mark(x.X)
				case *ast.UnaryExpr:
					if x.Op == token.AND {
						mark(x.X)
					}
				}
				return true
			})
		}
	}
	names := make([]string, 0, len(mutated))
	for name := range mutated {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		t.Errorf("%s/%s is package state, changed at %s: it belongs in the app", cli, name, strings.Join(mutated[name], ", "))
	}
}

// root is the variable an assignment target or an address starts from: x in x,
// x.f.g, x[i] and (*x).
func root(e ast.Expr) string {
	for {
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name
		case *ast.SelectorExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		default:
			return ""
		}
	}
}

// locals are the names a function declares itself: its parameters, results and
// receiver, and its own variables, which shadow the package's.
func locals(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	add := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			for _, n := range f.Names {
				out[n.Name] = true
			}
		}
	}
	add(fn.Recv)
	add(fn.Type.Params)
	add(fn.Type.Results)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				for _, l := range x.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						out[id.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for _, id := range x.Names {
				out[id.Name] = true
			}
		case *ast.FuncLit:
			add(x.Type.Params)
			add(x.Type.Results)
		case *ast.RangeStmt:
			if x.Tok == token.DEFINE {
				for _, e := range []ast.Expr{x.Key, x.Value} {
					if id, ok := e.(*ast.Ident); ok {
						out[id.Name] = true
					}
				}
			}
		}
		return true
	})
	return out
}
