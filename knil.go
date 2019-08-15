package knil

import (
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

var Analyzer = &analysis.Analyzer{
	Name: "knil",
	Doc:  Doc,
	Run:  run,
	Requires: []*analysis.Analyzer{
		inspect.Analyzer,
	},
}

const Doc = "Knil is static analyzer for kil0ing nil pointer dereference."

var valAsgnPos map[string]token.Pos

func run(pass *analysis.Pass) (interface{}, error) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	setAsgnPos(pass, inspect)
	checkDerefNonNil(pass, inspect)

	return nil, nil
}

func setAsgnPos(pass *analysis.Pass, inspect *inspector.Inspector) {
	valAsgnPos = map[string]token.Pos{}
	nodeFilter := []ast.Node{
		(*ast.AssignStmt)(nil),
	}
	inspect.Preorder(nodeFilter, func(n ast.Node) {
		asgnStmt, ok := n.(*ast.AssignStmt)
		if !ok {
			return
		}
		if asgnStmt.Rhs == nil {
			return
		}
		for _, l := range asgnStmt.Lhs {
			li, ok := l.(*ast.Ident)
			if !ok {
				// panic(fmt.Errorf("lhs is not an ident, but %#v", l))
				return
			}
			valAsgnPos[li.Name] = n.Pos()
		}
	})
}

func checkDerefNonNil(pass *analysis.Pass, inspect *inspector.Inspector) {
	nodeFilter := []ast.Node{
		(*ast.StarExpr)(nil),
	}
	inspect.Preorder(nodeFilter, func(n ast.Node) {
		starExp, ok := n.(*ast.StarExpr)
		if !ok {
			return
		}
		x, ok := starExp.X.(*ast.Ident)
		if !ok {
			return
		}
		if x.Pos() < valAsgnPos[x.Name] {
			pass.Reportf(n.Pos(), "%s is nil, but will be dereferenced", x.Name)
		}
	})
}
