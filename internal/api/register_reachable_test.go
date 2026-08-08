package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strings"
	"testing"
)

// Every route registration must be reachable from Register.
//
// This is the class one level up from "a verb is missing on a collection". A
// per-verb inventory catches a route that was never added to a registration it
// knows about; it cannot catch a WHOLE REGISTRATION that nobody calls, because
// there is then no collection for it to enumerate. `registerMLV` was one line
// away from exactly that, and the only reason it was caught is that its own
// route test happened to exist — which is not a mechanism, it is luck.
//
// The bespoke surfaces are the exposed ones: schedules, triggers, materialized
// lake views, Livy, high-concurrency Livy, KQL, labels, tenant settings, the
// admin routes. None of them belongs to a typed collection, so none appears in
// a typed inventory. Orphan any one of them and the emulator starts, serves
// everything else, and answers 404 on a surface the docs describe.
//
// Reading the package's own source is deliberate. The alternative — asserting
// one representative URL per surface — is a list that must be kept in step with
// another list, which is the failure this whole family is made of. Reachability
// is derived instead of declared, so a new `registerX` is covered the moment it
// exists.
func TestEveryRegistrationIsReachableFromRegister(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the package: %v", err)
	}
	pkg, ok := pkgs["api"]
	if !ok {
		t.Fatal("package api not found in .")
	}

	// defined: every method on *API named register… that takes a mux.
	// calls:   for each such method (and Register), the registrations it invokes.
	defined := map[string]bool{}
	calls := map[string][]string{}
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if strings.HasPrefix(name, "register") && takesMux(fn) {
				defined[name] = true
			}
			if name != "Register" && !strings.HasPrefix(name, "register") {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !strings.HasPrefix(sel.Sel.Name, "register") {
					return true
				}
				if ident, ok := sel.X.(*ast.Ident); !ok || ident.Name != "a" {
					return true
				}
				calls[name] = append(calls[name], sel.Sel.Name)
				return true
			})
		}
	}
	if len(defined) == 0 {
		t.Fatal("found no register… methods — the parse is wrong, not the code")
	}

	// Transitive, because registrations nest: registerLivy mounts the
	// high-concurrency routes, and a check that only read Register's own body
	// would report that one orphaned when it is not.
	reached := map[string]bool{}
	queue := append([]string(nil), calls["Register"]...)
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		if reached[fn] {
			continue
		}
		reached[fn] = true
		queue = append(queue, calls[fn]...)
	}

	orphans := []string{}
	for name := range defined {
		if !reached[name] {
			orphans = append(orphans, name)
		}
	}
	sort.Strings(orphans)
	if len(orphans) > 0 {
		t.Fatalf("these route registrations are never reached from Register, so every URL "+
			"they mount answers 404 while the emulator otherwise starts and serves normally: %v",
			orphans)
	}
}

func takesMux(fn *ast.FuncDecl) bool {
	for _, p := range fn.Type.Params.List {
		star, ok := p.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if ok && sel.Sel.Name == "ServeMux" {
			return true
		}
	}
	return false
}
