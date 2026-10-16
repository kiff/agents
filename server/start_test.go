package server_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Every route starts its runs through Server.start, which holds the tenant's
// cap: a route that called Runner.Start itself would start runs past it,
// and no list of routes in a test would notice a new one.
func TestEveryStartIsCapped(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := false
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Start" {
					return true
				}
				if recv, ok := sel.X.(*ast.SelectorExpr); ok && recv.Sel.Name == "Runner" {
					found = true
					if fn.Name.Name != "start" {
						t.Errorf("%s: %s calls Runner.Start itself, past the tenant's cap: call s.start", fset.Position(call.Pos()), fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	if !found {
		t.Fatal("no call to Runner.Start found: the check looks for the wrong thing")
	}
}
