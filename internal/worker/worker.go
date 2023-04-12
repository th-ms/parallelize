// Package worker implements AST-walk and manipulation functionalities
package worker

import (
	"context"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go/ast"
	"go/printer"
	"go/token"
	"go/types"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/packages"
	"os"
	"strings"
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
			}
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

	lg.Trace().Msgf("Checking for t.Run calls")
	var parallelized bool
	astutil.Apply(fdecl, func(c *astutil.Cursor) bool {
		if c.Node() == nil {
			lg.Trace().Msg("Skipping nil node")
			return false
		}

		truncall, ok := w.isTestingRunCall(ctx, c.Node())
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
		// NOTE: hopefully, this (i.e. returning false) does what we think it does
		return false
	}, nil)

	return parallelized
}

func (w Worker) isTestingRunCall(ctx context.Context, node ast.Node) (*ast.CallExpr, bool) {
	lg := log.Ctx(ctx)

	stmt, ok := node.(ast.Stmt)
	if !ok {
		lg.Trace().Msgf("Node skipped: Node %q is not an *ast.Stmt", w.fset.Position(node.Pos()))
		return nil, false
	}

	estmt, ok := stmt.(*ast.ExprStmt)
	if !ok {
		lg.Trace().Msgf("Node skipped: Statement %q is not an *ast.ExprStmt", w.fset.Position(stmt.Pos()))
		return nil, false
	}
	cexpr, ok := estmt.X.(*ast.CallExpr)
	if !ok {
		lg.Trace().Msgf("Node skipped: Expression statement does not point to an *ast.CallExpr", w.fset.Position(estmt.X.Pos()))
		return nil, false
	}
	fexpr, ok := cexpr.Fun.(*ast.SelectorExpr)
	if !ok {
		lg.Fatal().Msgf("I didn't know this could happen: Call expression function is not a *ast.SelectorExpr", w.fset.Position(cexpr.Fun.Pos()))
	}
	xident, ok := fexpr.X.(*ast.Ident)
	if !ok {
		lg.Trace().Msgf("I didn't know this could happen: Function expression does not point to an identifier", w.fset.Position(fexpr.X.Pos()))
	}
	if xident.Name != "t" {
		lg.Trace().Msgf("Node skipped: Function expression identifier name is %q, expected 't'", xident.Name)
		return nil, false
	}
	if fexpr.Sel.Name != "Run" {
		lg.Trace().Msgf("Node skipped: Function selector identifier name is %q, expected 'Run'", fexpr.Sel.Name)
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
