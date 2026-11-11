package app

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The layers of internal/ (docs/architektur.md), from the bottom up: a package may import only packages of its own or a lower layer. Every Go file under cmd/ and internal/ is parsed for its imports (standard library only, no go/packages), so a new package lands in a layer by its path and a wrong import names the file.
const modulePath = "github.com/pfisterer/llm-management-api/"

// layerOf is the layer of a package by its path relative to the module root; -1 is a path no layer knows, which is a failure of its own.
func layerOf(rel string) int {
	switch {
	case rel == "internal/generated_docs", strings.HasPrefix(rel, "internal/platform/"):
		return 0
	case rel == "internal/access", rel == "internal/roleprovider", strings.HasPrefix(rel, "internal/roleprovider/"):
		return 1
	case strings.HasPrefix(rel, "internal/llm/"), strings.HasPrefix(rel, "internal/cluster/"):
		return 2
	case rel == "internal/webserver":
		return 3
	case rel == "internal", rel == "cmd":
		return 4
	}
	return -1
}

func TestLayersImportDownwardsOnly(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	var problems []string
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			rel := filepath.ToSlash(filepath.Dir(strings.TrimPrefix(path, root+string(filepath.Separator))))
			from := layerOf(rel)
			if from < 0 {
				problems = append(problems, rel+": not in any layer of docs/architektur.md")
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, imp := range f.Imports {
				target, _ := strconv.Unquote(imp.Path.Value)
				if !strings.HasPrefix(target, modulePath) {
					continue
				}
				dep := strings.TrimPrefix(target, modulePath)
				to := layerOf(dep)
				switch {
				case to < 0:
					problems = append(problems, strings.TrimPrefix(path, root+"/")+": imports "+dep+", which is in no layer")
				case to > from:
					problems = append(problems, strings.TrimPrefix(path, root+"/")+": imports "+dep+" from a higher layer")
				case dep == "internal/generated_docs" && rel != "internal":
					problems = append(problems, strings.TrimPrefix(path, root+"/")+": imports generated_docs, which only app may")
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}
