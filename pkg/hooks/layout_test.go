package hooks

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestPublicPackagesImportNothingInternal keeps pkg/ importable on its own:
// a pkg/ package that imports internal/ can expose an internal type in its
// API, which programs outside this module can then neither name nor build.
func TestPublicPackagesImportNothingInternal(t *testing.T) {
	const internalPrefix = "github.com/discobox-ai/hooks/internal/"
	fset := token.NewFileSet()
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(imp, internalPrefix) {
				t.Errorf("%s imports %s; pkg/ packages must not depend on internal/", filepath.ToSlash(path), imp)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
