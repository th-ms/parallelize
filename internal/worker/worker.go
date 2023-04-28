// Package worker implements AST-walk and manipulation functionalities
package worker

import (
	"context"
	"go/ast"
	"go/printer"
	"go/token"
	"go/types"
	"os"
	"strings"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/packages"
)

// Worker is a terrible generic loaded term for this. This struct holds the necessary information
// for reading and manipulating packages in order to parallelize the unit tests
type Worker struct {
	fset *token.FileSet
	pkg  *packages.Package
}

func New(pkg *packages.Package, fset *token.FileSet) Worker {
	return Worker{
		pkg:  pkg,
		fset: fset,
	}
}

// Run ... TODO: what is this doing again?
func (w Worker) Run(ctx context.Context) {
	lg := log.Ctx(ctx)
	for _, fast := range w.pkg.Syntax {
		srcPath := w.fset.Position(fast.Package).Filename
		lg.UpdateContext(func(c zerolog.Context) zerolog.Context {
			return c.Str("src-path", srcPath)
		})
		if !isTestFile(srcPath) {
			lg.Trace().Msg("Skipping file due to not being a test file")
			continue
		}
		lg.Trace().Msg("Processing test file")
		w.processTestFile(ctx, fast)
		err := printer.Fprint(os.Stdout, w.fset, fast)
		if err != nil {
			lg.Fatal().Msg(err.Error())
		}
	}
}

func (w Worker) processTestFile(ctx context.Context, fast *ast.File) {
	astutil.Apply(fast, nil, func(c *astutil.Cursor) bool {
		n := c.Node()
		if fdecl, ok := n.(*ast.FuncDecl); ok {
			lg := log.Ctx(ctx).With().Str("func-name", fdecl.Name.Name).Logger()
			ctx = lg.WithContext(ctx)
			lg.UpdateContext(func(c zerolog.Context) zerolog.Context {
				return c.Str("func-name", fdecl.Name.Name)
			})
			if !w.isTestFunc(ctx, fdecl) {
				return true
			}

			if ok := w.maybeParallelizeRunCall(ctx, fdecl); !ok {
				fdecl.Body.List = prependParallelCall(fdecl.Body.List)
				lg.Trace().Msg("t.Parallel call added to simple test body")
				return true
			}
			// Parallelizing test tables can lead to some issues, let's fix them
			w.fixTestTables(ctx, fdecl)
		}
		return true
	})
}

func (w Worker) isTestFunc(ctx context.Context, fdecl *ast.FuncDecl) bool {
	lg := log.Ctx(ctx)
	lg.Trace().Msg("Checking if it's a test function")

	if !strings.HasPrefix(fdecl.Name.Name, "Test") {
		lg.Trace().Msg("Not a testing function: name does not start with 'Test'")
	}

	if len(fdecl.Type.Params.List) != 1 {
		lg.Trace().Msg("Not a test function: param list length is not one.")
	}

	cursor := fdecl.Type.Params.List[0].Type
	switch texpr := cursor.(type) {
	case *ast.StarExpr:
		switch ptexpr := w.pkg.TypesInfo.Types[texpr.X].Type.(type) {
		case *types.Named:
			if ptexpr.String() == "testing.T" {
				lg.Trace().Msg("Is a test function.")
				return true
			}
			lg.Trace().Msg("Not a test function: param is not testing.T")
			return false
		default:
			lg.Trace().Msgf("Not a test function: param is a pointer to %v", ptexpr)
			return false
		}
	default:
		lg.Trace().Msgf("Not a test function: param is not a pointer")
		return false
	}
}

// checks if test function has t.Run calls and parallelize them. Returns false if there's no t.Run call
func (w Worker) maybeParallelizeRunCall(ctx context.Context, fdecl *ast.FuncDecl) bool {
	lg := log.Ctx(ctx)

	lg.Trace().Msg("Checking for t.Run calls")
	var parallelized bool
	astutil.Apply(fdecl, func(c *astutil.Cursor) bool {
		if c.Node() == nil {
			return false
		}

		truncall, ok := w.isTestingRunCall(c.Node())
		if !ok {
			return true
		}

		if len(truncall.Args) != 2 {
			lg.Error().Msgf("Found t.Run call, but number of arguments is %d instead of 2", len(truncall.Args))
			return false
		}
		fn, ok := truncall.Args[1].(*ast.FuncLit)
		if !ok {
			lg.Error().Msgf("Found t.Run call, but the second argument is %T instead of *ast.FuncLit", truncall.Args[1])
			return false
		}

		fn.Body.List = prependParallelCall(fn.Body.List)
		parallelized = true

		// for this sub-tree, we already found t.Run
		// we are not supporting nested t.Runs
		// NOTE: hopefully, returning false here does what we think it does
		return false
	}, nil)

	return parallelized
}

func (w Worker) fixTestTables(ctx context.Context, fdecl *ast.FuncDecl) bool {
	lg := log.Ctx(ctx)

	lg.Trace().Msg("Checking for test table")
	lg.Trace().Msg("Checking for 'tests' var declaration")
	stmts := fdecl.Body.List
	hasTestsVar := false
	for _, stmt := range stmts {
		astmt, ok := stmt.(*ast.AssignStmt)
		if !ok {
			continue
		}
		if len(astmt.Lhs) != 1 {
			continue
		}
		ident, ok := astmt.Lhs[0].(*ast.Ident)
		if !ok {
			continue
		}
		if ident.Name != "tests" {
			continue
		}
		hasTestsVar = true
	}
	if !hasTestsVar {
		lg.Trace().Msg("No 'tests' variable found. Quitting...")
		return false
	}

	lg.Trace().Msg("Checking for table test loop")
	for _, stmt := range stmts {
		rstmt, ok := stmt.(*ast.RangeStmt)
		if !ok {
			continue
		}
		ident, ok := rstmt.X.(*ast.Ident)
		if !ok {
			continue
		}

		if ident.Name != "tests" {
			continue
		}

		for _, stmt := range rstmt.Body.List {
			_, ok := w.isTestingRunCall(stmt)
			if !ok {
				continue
			}

			lg.Trace().Msg("Found loop for table test")
			// Pin tt variable
			// See: https://gist.github.com/posener/92a55c4cd441fc5e5e85f27bca008721
			rstmt.Body.List = append([]ast.Stmt{&ast.AssignStmt{
				Lhs: []ast.Expr{
					&ast.Ident{
						Name: "tt",
					},
				},
				Rhs: []ast.Expr{
					&ast.Ident{
						Name: "tt",
					},
				},
				Tok: token.DEFINE,
			}}, rstmt.Body.List...)
		}

	}

	return false
}

func (w Worker) isTestingRunCall(node ast.Node) (*ast.CallExpr, bool) {
	stmt, ok := node.(ast.Stmt)
	if !ok {
		return nil, false
	}

	estmt, ok := stmt.(*ast.ExprStmt)
	if !ok {
		return nil, false
	}
	cexpr, ok := estmt.X.(*ast.CallExpr)
	if !ok {
		return nil, false
	}
	fexpr, ok := cexpr.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	xident, ok := fexpr.X.(*ast.Ident)
	if !ok {
		return nil, false
	}
	if xident.Name != "t" {
		return nil, false
	}
	if fexpr.Sel.Name != "Run" {
		return nil, false
	}

	return cexpr, true
}

func isTestFile(s string) bool {
	return strings.HasSuffix(s, "_test.go")
}

func prependParallelCall(stmts []ast.Stmt) []ast.Stmt {
	return append([]ast.Stmt{&ast.ExprStmt{
		X: &ast.CallExpr{
			Fun: &ast.SelectorExpr{
				X: &ast.Ident{
					Name: "t",
				},
				Sel: &ast.Ident{
					Name: "Parallel",
				},
			},
		},
	}}, stmts...)
}
