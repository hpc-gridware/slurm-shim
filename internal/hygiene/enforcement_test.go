package hygiene

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStaticBuildFlags asserts the release build stays static and dependency-free
// beyond the GE clients (REQ-DLV-002): CGO disabled and the pure-Go osusergo and
// netgo tags, so getent/DNS never pull in libc.
func TestStaticBuildFlags(t *testing.T) {
	root := repoRoot(t)
	mk, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(mk)
	for _, want := range []string{"CGO_ENABLED=0", "osusergo", "netgo"} {
		if !strings.Contains(s, want) {
			t.Errorf("Makefile build recipe is missing %q (REQ-DLV-002 static build)", want)
		}
	}
}

// TestOsExecConfined asserts no production package outside the sanctioned set
// imports os/exec (REQ-IMP-001): external process execution is routed through the
// gedata.Runner and launch.Launcher seams; the stepper is the third point
// because it forks ranks (the D-3 trampoline).
func TestOsExecConfined(t *testing.T) {
	root := repoRoot(t)
	allowed := map[string]bool{
		"internal/gedata":  true,
		"internal/launch":  true,
		"internal/stepper": true,
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "testdata", "tools":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(src), `"os/exec"`) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		pkgDir := filepath.ToSlash(filepath.Dir(rel))
		if !allowed[pkgDir] {
			t.Errorf("%s imports os/exec outside the sanctioned packages (REQ-IMP-001)", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestDependencyAllowlist keeps the direct dependency surface minimal and
// reviewed (REQ-IMP-002): only the testing framework, flag parser, yaml, and the
// x/sys syscall shim are permitted as direct requires.
func TestDependencyAllowlist(t *testing.T) {
	root := repoRoot(t)
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"github.com/onsi/ginkgo/v2": true,
		"github.com/onsi/gomega":    true,
		"github.com/spf13/pflag":    true,
		"golang.org/x/sys":          true,
		"gopkg.in/yaml.v3":          true,
	}
	inBlock := false
	for _, line := range strings.Split(string(mod), "\n") {
		t2 := strings.TrimSpace(line)
		switch {
		case t2 == "require (":
			inBlock = true
		case inBlock && t2 == ")":
			inBlock = false
		case inBlock && t2 != "" && !strings.Contains(t2, "// indirect"):
			mod := strings.Fields(t2)[0]
			if !allowed[mod] {
				t.Errorf("unreviewed direct dependency %q in go.mod (REQ-IMP-002)", mod)
			}
		}
	}
}
