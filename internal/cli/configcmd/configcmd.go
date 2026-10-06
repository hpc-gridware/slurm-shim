// Package configcmd is `slurm-shim config`: two small machine interfaces so a
// tool that manages the config (Qontrol, a script) never re-implements the
// shim's search order or its schema rules. `config path` says which file this
// host loads and why; `config check` validates a candidate before it is saved.
// Both are offline: no cluster call, nothing executed.
package configcmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/hpc-gridware/slurm-shim/internal/config"
)

// schemaVersion is bumped when a JSON shape below changes incompatibly.
const schemaVersion = 1

// maxConfigBytes bounds what `config check` reads: a config is a few KiB, and a
// check run on every save must not be made to swallow an arbitrary stream.
const maxConfigBytes = 4 << 20

const usage = "usage: slurm-shim config path [--json] | slurm-shim config check FILE|- [--json]"

// PathResult is `config path --json`.
type PathResult struct {
	SchemaVersion int    `json:"schema_version"`
	Path          string `json:"path"`
	Source        string `json:"source"`
	Exists        bool   `json:"exists"`
	Pointer       string `json:"pointer,omitempty"`
	Error         string `json:"error,omitempty"`
}

// CheckResult is `config check --json`.
type CheckResult struct {
	SchemaVersion int      `json:"schema_version"`
	Valid         bool     `json:"valid"`
	Errors        []string `json:"errors"`
	Warnings      []string `json:"warnings"`
}

// Run is the entry point. Exit 0 ok, 1 invalid or unresolvable, 2 usage.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	asJSON := false
	var rest []string
	for _, a := range args {
		if a == "--json" || a == "-json" {
			asJSON = true
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) == 0 {
		fmt.Fprintln(stderr, "config: error: "+usage)
		return 2
	}
	switch rest[0] {
	case "path":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "config: error: "+usage)
			return 2
		}
		return runPath(config.SelfPrefix(), asJSON, stdout)
	case "check":
		if len(rest) != 2 {
			fmt.Fprintln(stderr, "config: error: "+usage)
			return 2
		}
		return runCheck(rest[1], stdin, asJSON, stdout, stderr)
	}
	fmt.Fprintf(stderr, "config: error: unknown subcommand %q; %s\n", rest[0], usage)
	return 2
}

func runPath(prefix string, asJSON bool, stdout io.Writer) int {
	loc, err := config.Resolve(prefix)
	res := PathResult{SchemaVersion: schemaVersion, Path: loc.Path, Source: loc.Source, Exists: loc.Exists,
		Pointer: loc.Pointer}
	code := 0
	if err != nil {
		res.Error, code = err.Error(), 1
	}
	if asJSON {
		writeJSON(stdout, res)
		return code
	}
	switch {
	case err != nil:
		fmt.Fprintf(stdout, "error: %v\n", err)
	case !loc.Exists:
		fmt.Fprintln(stdout, "(no config file: compiled-in defaults)")
	case loc.Source == config.SourcePointer:
		fmt.Fprintf(stdout, "%s (pointer %s)\n", loc.Path, loc.Pointer)
	default:
		fmt.Fprintf(stdout, "%s (%s)\n", loc.Path, loc.Source)
	}
	return code
}

// runCheck runs exactly what every command runs on its config, so a file it
// accepts is one a job accepts: errors are what config.Parse rejects, warnings
// what it reports and carries on past.
func runCheck(file string, stdin io.Reader, asJSON bool, stdout, stderr io.Writer) int {
	data, err := readCandidate(file, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "config: error: %v\n", err)
		return 2
	}
	res := CheckResult{SchemaVersion: schemaVersion, Valid: true, Errors: []string{}, Warnings: []string{}}
	_, warns, err := config.Parse(data)
	if err != nil {
		res.Valid = false
		res.Errors = append(res.Errors, err.Error())
	} else {
		res.Warnings = append(res.Warnings, warns...)
	}
	if asJSON {
		writeJSON(stdout, res)
	} else {
		for _, e := range res.Errors {
			fmt.Fprintf(stdout, "error: %s\n", e)
		}
		for _, w := range res.Warnings {
			fmt.Fprintf(stdout, "warning: %s\n", w)
		}
		if res.Valid {
			fmt.Fprintln(stdout, "valid")
		} else {
			fmt.Fprintln(stdout, "invalid")
		}
	}
	if !res.Valid {
		return 1
	}
	return 0
}

func readCandidate(file string, stdin io.Reader) ([]byte, error) {
	r := stdin
	if file != "-" {
		f, err := os.Open(file)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	data, err := io.ReadAll(io.LimitReader(r, maxConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxConfigBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes; not a slurm-shim config", file, maxConfigBytes)
	}
	return data, nil
}

func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v) // a write error to stdout has nowhere better to go
}
