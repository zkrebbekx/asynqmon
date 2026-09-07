package asynqmon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

// ****************************************************************************
// Guard test for #53 item 5. net/http recovers a panic only on the goroutine
// that serves the request, so a bare "go" statement anywhere in the
// repository can end the process. Every goroutine must start through
// internal/safego instead. This test walks the module and fails on a bare
// "go" statement in non-test code.
// ****************************************************************************

// goStmtAllowlist lists the non-test files that may contain a bare "go"
// statement, with the reason. Keep it short.
var goStmtAllowlist = map[string]string{
	// safego.Go is the one place that starts a goroutine; it starts
	// safego.Run, which holds the recover.
	filepath.Join("internal", "safego", "safego.go"): "the recovery helper itself",
}

func TestNoBareGoStatements(t *testing.T) {
	Convey("Given every non-test Go file of the repository", t, func() {
		var offenders []string
		fset := token.NewFileSet()

		err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "ui", "node_modules", "testdata", ".git":
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			if _, ok := goStmtAllowlist[filepath.Clean(path)]; ok {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return perr
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if g, ok := n.(*ast.GoStmt); ok {
					offenders = append(offenders, fset.Position(g.Go).String())
				}
				return true
			})
			return nil
		})

		Convey("When the files are parsed", func() {
			So(err, ShouldBeNil)

			Convey("Then no file starts a goroutine outside internal/safego", func() {
				So(strings.Join(offenders, "\n"), ShouldBeBlank)
			})
		})
	})
}
