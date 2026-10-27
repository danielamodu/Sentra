package verify

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestPackageIndependence enforces the independence boundary statically: the
// verifier may import sentra/domain and the standard library, nothing else.
//
// This is deliberately an AST scan of the source files rather than a check of
// compiled dependency graphs, so the boundary holds even for test-only helpers:
// a verifier test that imported sim or engine could reconstruct reference
// mechanics and quietly turn agreement into an assumption.
func TestPackageIndependence(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse verify package: %v", err)
	}

	forbidden := map[string]string{
		"sentra/sim":      "reference system execution mechanics",
		"sentra/engine":   "reference event engine",
		"sentra/evidence": "serializer/harness that imports the reference system",
	}

	var violations []string
	scanned := 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			scanned++
			for _, spec := range file.Imports {
				path := strings.Trim(spec.Path.Value, `"`)
				if path == "sentra/domain" || isStdlibPath(path) {
					continue
				}
				why := "not part of the verifier whitelist"
				if f, ok := forbidden[path]; ok {
					why = f
				}
				violations = append(violations, fmt.Sprintf("%s imports %q (%s)", name, path, why))
			}
		}
	}

	if scanned == 0 {
		t.Fatal("boundary test scanned no files; it cannot certify independence")
	}
	// ParseDir only descends one directory. Assert that the scan really covers
	// every Go file under verify/, so a future subpackage cannot escape the
	// boundary by moving out of the package directory.
	if want := countGoFiles(t, "."); scanned != want {
		t.Fatalf("boundary test scanned %d Go files, but %d exist under verify/; the scan is incomplete", scanned, want)
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("verifier independence violated:\n  %s", strings.Join(violations, "\n  "))
	}
}

// countGoFiles counts every .go file under root, recursively.
func countGoFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".go") {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if n == 0 {
		t.Fatalf("found no Go files under %s", root)
	}
	return n
}

// TestReferencePackagesAreForbidden names the reference packages explicitly so a
// future whitelist edit cannot silently re-admit them.
func TestReferencePackagesAreForbidden(t *testing.T) {
	for _, path := range []string{"sentra/sim", "sentra/engine", "sentra/evidence"} {
		if path == "sentra/domain" || isStdlibPath(path) {
			t.Fatalf("%q must not be an allowed verifier import", path)
		}
	}
	if !isStdlibPath("crypto/sha256") || !isStdlibPath("go/parser") {
		t.Fatal("stdlib detection rejects standard packages")
	}
	if isStdlibPath("sentra/domain") || isStdlibPath("github.com/example/pkg") {
		t.Fatal("stdlib detection accepts module paths")
	}
}

// isStdlibPath reports whether an import path belongs to the standard library:
// standard package paths have no dot in their first path element, and this
// module's own paths are never standard library.
func isStdlibPath(path string) bool {
	if strings.HasPrefix(path, "sentra/") {
		return false
	}
	first := path
	if i := strings.IndexByte(path, '/'); i >= 0 {
		first = path[:i]
	}
	return !strings.Contains(first, ".")
}
