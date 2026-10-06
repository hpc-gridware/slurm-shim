package installcmd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/install"
)

// movedSuffix marks a cell config that was moved away, so nobody edits a copy
// that is no longer read.
const movedSuffix = ".moved-to-config-dir"

// Sources of a config target besides the config.Source* ones.
const (
	sourceFlag      = "flag"
	sourceConfigDir = "config-dir"
)

// configTarget is the config an install writes and what that changes.
type configTarget struct {
	Path   string
	Source string // sourceFlag, sourceConfigDir, or config.SourceEnv/Pointer/Cell
	// Dir is set when Path is the config in a Qontrol config dir: every read
	// and write then goes through the dir's os.Root (install.ConfigDir).
	Dir *install.ConfigDir
	// Previous is the config the install's jobs read today when this run
	// moves them to Path, "" otherwise. It is merged in when Path does not
	// exist yet, and retired once the pointer names Path.
	Previous string
	// WritePointer is true when the pointer must name Path after this run. A
	// config chosen by $SLURM_SHIM_CONFIG never becomes permanent, and the cell
	// path needs no pointer.
	WritePointer bool
}

// resolveConfigTarget decides, from the flags and the install at prefix,
// which config this install writes. The int is the exit code for an error.
func resolveConfigTarget(cfgFlag, cfgDirFlag, prefix string, euid int) (configTarget, int, error) {
	var t configTarget
	switch {
	case cfgFlag != "" && cfgDirFlag != "":
		return t, 2, errors.New("--config and --config-dir are mutually exclusive")
	case cfgDirFlag != "":
		cd, err := install.CheckConfigDir(cfgDirFlag, cellCommon(), euid)
		if err != nil {
			return t, 2, err
		}
		t.Path, t.Source, t.Dir = cd.Config, sourceConfigDir, &cd
	case cfgFlag == "":
		p, src, err := config.InstallPath(prefix)
		if err != nil {
			return t, 1, err
		}
		t.Path, t.Source = p, src
	default:
		abs, err := filepath.Abs(cfgFlag)
		if err != nil {
			return t, 2, fmt.Errorf("--config: %w", err)
		}
		t.Path, t.Source = abs, sourceFlag
	}
	// However it was named -- followed through the pointer on a re-run, or as
	// --config -- a config in a Qontrol config dir gets the same checks and the
	// same handling, so a re-run as root never takes it from Qontrol's user.
	if t.Dir == nil {
		if cd, ok := install.OpenConfigDir(t.Path, euid); ok {
			checked, err := install.CheckConfigDir(cd.Dir, cellCommon(), euid)
			if err != nil {
				return t, 2, err
			}
			t.Dir = &checked
		}
	}
	t.WritePointer = t.Source != config.SourceEnv && t.Path != config.CellPath()
	if t.Source == sourceFlag || t.Source == sourceConfigDir {
		current, _, err := config.InstallDefault(prefix)
		if err != nil {
			return t, 1, err
		}
		if !install.SamePath(current, t.Path) {
			t.Previous = current
		}
	}
	return t, 0, nil
}

// readConfig reads a config as the installer: through the Qontrol config
// dir's os.Root when it lives in one, never by a path its owner can redirect.
func readConfig(path string, t configTarget) ([]byte, error) {
	if cd, ok := install.OpenConfigDir(path, configDirEUID(t)); ok {
		return cd.ReadConfig()
	}
	return os.ReadFile(path)
}

// writeConfig writes the target config: through its config dir, else
// atomically in place.
func writeConfig(t configTarget, data []byte) error {
	if t.Dir != nil {
		return t.Dir.WriteConfig(data)
	}
	if err := os.MkdirAll(filepath.Dir(t.Path), 0o755); err != nil {
		return err
	}
	return writeConfigAtomic(t.Path, data)
}

// loadConfig parses the first existing of paths, returning which one it was
// ("" when none exists, with nil config). A file that exists but cannot be
// read or parsed is an error naming it.
func loadConfig(paths []string, t configTarget) (*config.Config, string, error) {
	for _, p := range paths {
		data, err := readConfig(p, t)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, p, err
		}
		cfg, _, err := config.Parse(data)
		if err != nil {
			return nil, p, fmt.Errorf("not valid YAML: %w", err)
		}
		return cfg, p, nil
	}
	return nil, "", nil
}

func configDirEUID(t configTarget) int {
	if t.Dir != nil {
		return t.Dir.EUID
	}
	return os.Geteuid()
}

// cellCommon is $SGE_ROOT/$SGE_CELL/common, "" without $SGE_ROOT.
func cellCommon() string {
	if os.Getenv("SGE_ROOT") == "" {
		return ""
	}
	return filepath.Dir(filepath.Dir(config.CellPath()))
}

// readerOf names another install (not the tree at prefix, when prefix is set)
// that reads path as its config, "" when none does. An install whose pointer
// cannot be read is counted as a reader: removing or moving a file it might
// use is not worth the risk.
func readerOf(path, prefix string, f install.Facts) string {
	inTree := func(string) bool { return false }
	if prefix != "" {
		inTree = install.TreeMatcher(prefix)
	}
	for _, ref := range install.ShimInstalls(f) {
		if inTree(filepath.Join(ref.Prefix, install.BinaryRel)) {
			continue
		}
		cfg, _, err := config.InstallDefault(ref.Prefix)
		if err != nil || install.SamePath(cfg, path) {
			return ref.Object
		}
	}
	return ""
}

// retireMovedConfig deals with the config the install's jobs read until now.
// A cell config is renamed so nobody edits a file that is no longer read --
// unless another install in the cluster still reads it. Any other file is left
// where it is, and said so.
func retireMovedConfig(from, prefix string, f install.Facts, stdout, stderr io.Writer) {
	if _, err := os.Lstat(from); err != nil {
		return
	}
	if from != config.CellPath() {
		fmt.Fprintf(stdout, "config     %s left in place; this install no longer reads it\n", from)
		return
	}
	if other := readerOf(from, prefix, f); other != "" {
		fmt.Fprintf(stdout, "config     %s kept: another slurm-shim install still reads it (%s)\n", from, other)
		return
	}
	if err := os.Rename(from, from+movedSuffix); err != nil {
		fmt.Fprintf(stderr, "install: warning: renaming %s: %v; it is no longer read, so remove it "+
			"before someone edits it\n", from, err)
		return
	}
	fmt.Fprintf(stdout, "config     %s renamed to %s (no longer read)\n", from, filepath.Base(from+movedSuffix))
}

// updatePointer makes the install's pointer agree with the config it wrote:
// written when the config is outside the cell, removed when it is back at the
// cell path. A config chosen by $SLURM_SHIM_CONFIG leaves the pointer as it is.
func updatePointer(prefix string, t configTarget, stdout, stderr io.Writer) int {
	pointer := filepath.Join(prefix, config.PointerRel)
	if t.WritePointer {
		if err := config.WritePointer(prefix, t.Path); err != nil {
			fmt.Fprintf(stderr, "install: error: writing %s: %v; jobs still read the previous config\n", pointer, err)
			return 1
		}
		fmt.Fprintf(stdout, "pointer    %s -> %s written\n", pointer, t.Path)
		return 0
	}
	if t.Source == config.SourceEnv {
		return 0
	}
	if _, err := os.Lstat(pointer); err != nil {
		return 0
	}
	if err := config.RemovePointer(prefix); err != nil {
		fmt.Fprintf(stderr, "install: error: removing %s: %v\n", pointer, err)
		return 1
	}
	fmt.Fprintf(stdout, "pointer    %s removed (the config is at the cell path)\n", pointer)
	return 0
}
