package anansistore

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestNoPooledDocumentEscapes is the build guard for the pooled-document
// rule (Store specification §9): documents anansi hands out own pooled
// containers that Release must return before the pool reuses them, so a
// document value may never cross a function boundary — not in an exported
// signature, not in any struct field. Documents stay function-local inside
// the helpers that Release them. The test parses this package's non-test
// sources with the stdlib parser; if an anansi document type ever surfaces
// in an exported signature or a type declaration, the build fails.
//
// Runtime use-after-release is covered separately: every repo path runs
// under TestContract and TestDifferential, and a released document reads as
// cleared data, which those comparisons reject.
func TestNoPooledDocumentEscapes(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("no sources parsed")
	}

	// documentType reports whether an expression mentions a pooled anansi
	// document type (data.Documenter, document.Document, *Document...).
	var documentType func(expr ast.Expr) string
	documentType = func(expr ast.Expr) string {
		switch e := expr.(type) {
		case *ast.SelectorExpr:
			if id, ok := e.X.(*ast.Ident); ok && id.Name == "data" && e.Sel.Name == "Documenter" {
				return "data.Documenter"
			}
			if e.Sel.Name == "Document" {
				if id, ok := e.X.(*ast.Ident); ok && id.Name == "document" {
					return "document.Document"
				}
			}
			return ""
		case *ast.StarExpr:
			return documentType(e.X)
		case *ast.ArrayType:
			return documentType(e.Elt)
		case *ast.MapType:
			if s := documentType(e.Key); s != "" {
				return s
			}
			return documentType(e.Value)
		case *ast.FuncType:
			if e.Params != nil {
				for _, f := range e.Params.List {
					if s := documentType(f.Type); s != "" {
						return s
					}
				}
			}
			if e.Results != nil {
				for _, f := range e.Results.List {
					if s := documentType(f.Type); s != "" {
						return s
					}
				}
			}
			return ""
		}
		return ""
	}

	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			rel := fset.Position(file.Pos()).Filename
			ast.Inspect(file, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.FuncDecl:
					if node.Recv != nil || !node.Name.IsExported() {
						return true // internal helpers release locally
					}
					var bad []string
					if node.Type.Params != nil {
						for _, f := range node.Type.Params.List {
							if s := documentType(f.Type); s != "" {
								bad = append(bad, s)
							}
						}
					}
					if node.Type.Results != nil {
						for _, f := range node.Type.Results.List {
							if s := documentType(f.Type); s != "" {
								bad = append(bad, s)
							}
						}
					}
					if len(bad) > 0 {
						t.Errorf("%s: exported func %s takes or returns pooled type(s) %v",
							rel, node.Name.Name, bad)
					}
				case *ast.TypeSpec:
					st, ok := node.Type.(*ast.StructType)
					if !ok || st.Fields == nil {
						return true
					}
					for _, f := range st.Fields.List {
						if s := documentType(f.Type); s != "" {
							t.Errorf("%s: type %s stores pooled type %s in a field",
								rel, node.Name.Name, s)
						}
					}
				}
				return true
			})
		}
	}
}
