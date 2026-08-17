// Package hygiene holds repo-wide source-convention guards (M8). It has no
// runtime code; the tests enforce project conventions across all Go sources.
package hygiene

import (
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from working dir")
		}
		dir = parent
	}
}

func firstNonASCII(s string) (rune, bool) {
	for _, r := range s {
		if r > 127 {
			return r, true
		}
	}
	return 0, false
}

// TestNoUnicodeInCode enforces the project convention: no non-ASCII bytes in Go
// code or comments. Unicode is allowed only inside string and rune literals
// (user-facing output), which are skipped here.
func TestNoUnicodeInCode(t *testing.T) {
	root := repoRoot(t)
	var violations []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		file := fset.AddFile(path, fset.Base(), len(src))
		var s scanner.Scanner
		s.Init(file, src, nil, scanner.ScanComments)
		for {
			pos, tok, lit := s.Scan()
			if tok == token.EOF {
				break
			}
			// Unicode is permitted only in string/rune literals.
			if tok == token.STRING || tok == token.CHAR {
				continue
			}
			if r, bad := firstNonASCII(lit); bad {
				rel, _ := filepath.Rel(root, path)
				violations = append(violations, fmt.Sprintf("%s:%d contains U+%04X (%q)", rel, fset.Position(pos).Line, r, r))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Fatalf("non-ASCII in Go code/comments (allowed only in string/rune literals):\n%s", strings.Join(violations, "\n"))
	}
}
