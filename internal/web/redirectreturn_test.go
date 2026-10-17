/*
	Copyright (C) 2026  Orsiris de Jong <ozy@netpower.fr>

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// A REDIRECT ENDS THE RESPONSE, SO IT MUST END THE HANDLER.
//
// http.Redirect writes the status line and the Location header immediately.
// Every later write to that ResponseWriter is discarded, silently -- no
// error, no panic, nothing in a log. So a redirect that is not the last
// thing a handler does leaves the operator reading an answer that was
// decided before the work was even attempted.
//
// That is not hypothetical. postScheduleSave warned "Saved, but the
// verification interval is shorter than the sync interval" and carried on
// WITHOUT returning -- through the shutdown-timeout parse, the settings
// read, the cadence validation, the audit append and SetScheduleEntry. Any
// of those can fail, and all of them then redirected into a flushed header.
// The operator was told the change was saved, the entry was never written,
// and the real refusal went nowhere: the VM kept replicating on its old
// cadence, or stayed unscheduled, while the console said otherwise.
//
// The rule is positional and mechanical, which is why it can be checked: a
// redirect must be followed immediately by a return, unless it is the last
// statement of the FUNCTION BODY.
//
// Last-in-its-own-block is NOT enough, and getting that wrong is how the
// first version of this test passed against the very defect it was written
// for -- the "Saved, but" redirect is the last statement of its `if` block,
// and execution carries straight on past the closing brace.
func TestEveryRedirectEndsItsHandler(t *testing.T) {
	const file = "handlers.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	isRedirect := func(n ast.Node) bool {
		es, ok := n.(*ast.ExprStmt)
		if !ok {
			return false
		}
		call, ok := es.X.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		// The redirect helpers, plus http.Redirect itself in case a
		// handler ever calls it directly.
		if strings.HasPrefix(sel.Sel.Name, "redirect") {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && pkg.Name == "http" && sel.Sel.Name == "Redirect"
	}

	found := 0
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			block, ok := n.(*ast.BlockStmt)
			if !ok {
				return true
			}
			for i, stmt := range block.List {
				if !isRedirect(stmt) {
					continue
				}
				found++
				last := i == len(block.List)-1
				// Only the function's OWN body ends the handler by running
				// out of statements. Falling off the end of an if or a loop
				// continues below it.
				if last && block == fn.Body {
					continue
				}
				if !last {
					if _, isReturn := block.List[i+1].(*ast.ReturnStmt); isReturn {
						continue
					}
				}
				t.Errorf("%s:%d: in %s, a redirect does not end the handler. http.Redirect has already written the status and Location, so everything after it is discarded in silence -- including any later redirect carrying the real outcome. If the message is a warning the handler should carry on past, collect it and fold it into the final redirect instead of sending it here.",
					file, fset.Position(stmt.Pos()).Line, fn.Name.Name)
			}
			return true
		})
	}

	// A walk that matched nothing would pass without checking anything,
	// which is how this kind of test rots into a no-op.
	if found < 20 {
		t.Fatalf("found only %d redirect statements in %s; this audit is no longer covering the handlers", found, file)
	}
}
