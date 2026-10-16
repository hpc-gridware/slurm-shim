/*___INFO__MARK_BEGIN__*/
/*************************************************************************
*  Copyright 2026 HPC-Gridware GmbH
*
*  Licensed under the Apache License, Version 2.0 (the "License");
*  you may not use this file except in compliance with the License.
*  You may obtain a copy of the License at
*
*      http://www.apache.org/licenses/LICENSE-2.0
*
*  Unless required by applicable law or agreed to in writing, software
*  distributed under the License is distributed on an "AS IS" BASIS,
*  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
*  See the License for the specific language governing permissions and
*  limitations under the License.
*
************************************************************************/
/*___INFO__MARK_END__*/

package hygiene

import (
	"os"
	"path/filepath"
	"regexp"
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
// reviewed (REQ-IMP-002): only the testing framework, flag parser, yaml, the
// x/sys syscall shim, and go-clusterscheduler (the shared OCS/GE command parser)
// are permitted as direct requires.
func TestDependencyAllowlist(t *testing.T) {
	root := repoRoot(t)
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"github.com/hpc-gridware/go-clusterscheduler": true, // OCS/GE command parsing (shared source of truth)
		"github.com/onsi/ginkgo/v2":                   true,
		"github.com/onsi/gomega":                      true,
		"github.com/spf13/pflag":                      true,
		"golang.org/x/sys":                            true,
		"gopkg.in/yaml.v3":                            true,
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

// TestCopyrightHeaders asserts every Go source and every script that ships in
// a release or is installed at a site carries the HPC-Gridware Apache-2.0
// header, in the ___INFO__MARK block OCS and go-clusterscheduler use. Any year
// passes: OCS stamps each file with the year it was created.
func TestCopyrightHeaders(t *testing.T) {
	root := repoRoot(t)
	marker := regexp.MustCompile(`Copyright 20[0-9]{2} HPC-Gridware GmbH`)
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "vendor" || d.Name() == "testdata" || d.Name() == "tools") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	scripts, err := filepath.Glob(filepath.Join(root, "docs", "install", "*.sh"))
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, scripts...)
	files = append(files, filepath.Join(root, "scripts", "install.sh"))
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !marker.Match(src) {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s has no HPC-Gridware copyright header", rel)
		}
	}
}
