// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoExternalToolDependency guards against a repeat of updater v0.2.3,
// which required the `gh` CLI for attestation verification. Service managers
// never have gh on their PATH (launchd's default PATH is
// /usr/bin:/bin:/usr/sbin:/sbin), so every node on pilotprotocol
// v1.12.2–v1.13.4 refused every update for as long as it ran that updater.
//
// The updater must verify and apply releases with no external tools. The one
// allowed exec is `launchctl` (restarting the daemon under launchd on
// macOS), and it must go through runCommand so tests can stub it. This test
// parses the package's non-test sources and fails on any other exec.
func TestNoExternalToolDependency(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var execCalls int
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if n.Kind == token.STRING {
					if v, _ := strconv.Unquote(n.Value); v == "gh" {
						t.Errorf("%s: string literal \"gh\" — the updater must not depend on the gh CLI", fset.Position(n.Pos()))
					}
				}
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "exec" {
					return true
				}
				execCalls++
				switch sel.Sel.Name {
				case "Command", "CommandContext", "LookPath":
				default:
					return true
				}
				// The single allowed exec lives in runCommand and takes its
				// program name from the caller.
				if fn := enclosingVar(f, n); fn != "runCommand" {
					t.Errorf("%s: exec.%s outside runCommand; the updater must not run external tools",
						fset.Position(n.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
	if execCalls != 1 {
		t.Errorf("found %d exec.* calls, want exactly 1 (runCommand)", execCalls)
	}

	// Every command actually run goes through Updater.command; the only
	// program named anywhere is launchctl.
	src, err := os.ReadFile("updater.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(src), `u.command(`); n != 1 || !strings.Contains(string(src), `u.command("launchctl"`) {
		t.Errorf("expected exactly one u.command(\"launchctl\", ...) call, found %d u.command calls", n)
	}
}

// enclosingVar returns the name of the package-level var whose initializer
// contains node, or "".
func enclosingVar(f *ast.File, node ast.Node) string {
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, v := range vs.Values {
				if v.Pos() <= node.Pos() && node.End() <= v.End() && i < len(vs.Names) {
					return vs.Names[i].Name
				}
			}
		}
	}
	return ""
}
