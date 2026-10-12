// Package slurmopts is SLURM's own option table: every option of every SLURM
// command the shim emulates, with whether it takes a value. It exists so that a
// parser can step over an option the shim does not translate without guessing
// its arity. A guess is how `sbatch --qos normal job.sh` submitted "normal" as
// the script, `srun -u python x.py` ran x.py, and `scancel -p batch` passed the
// partition name to qdel.
//
// The tables are generated from the SLURM man pages (see genopts) and embedded
// from slurm-<version>/<command>.txt, one option per line:
//
//	-A --account required
//	--exclusive optional
//	-W --wait none
//
// The table says nothing about what the shim does with an option; each
// command's parser decides that.
package slurmopts

import (
	"bufio"
	"embed"
	"fmt"
	"strings"
)

// Version is the SLURM release the embedded tables were generated from.
const Version = "24.05"

//go:embed slurm-24.05/*.txt
var tables embed.FS

// Kind is how an option takes a value.
type Kind int

const (
	// None takes no value (--wait).
	None Kind = iota
	// Required takes a value, attached (--account=a, -Aa) or as the next
	// token (--account a, -A a).
	Required
	// Optional takes a value only attached (--exclusive=user); in the space
	// form it consumes nothing, as getopt_long's optional_argument does.
	Optional
)

// String is the fixture spelling of the kind.
func (k Kind) String() string {
	switch k {
	case Required:
		return "required"
	case Optional:
		return "optional"
	}
	return "none"
}

// Option is one SLURM option: all its spellings (with dashes) and its kind.
type Option struct {
	Names []string
	Value Kind
}

// Table is one command's options, by spelling.
type Table struct {
	byName map[string]Option
}

// Commands are the SLURM commands with an embedded table.
var Commands = []string{"sbatch", "srun", "salloc", "squeue", "sacct", "scancel", "scontrol", "sinfo"}

var loaded = map[string]Table{}

func init() {
	for _, c := range Commands {
		data, err := tables.ReadFile("slurm-" + Version + "/" + c + ".txt")
		if err != nil {
			panic(fmt.Sprintf("slurmopts: missing table for %s: %v", c, err))
		}
		opts, err := ParseTable(string(data))
		if err != nil {
			panic(fmt.Sprintf("slurmopts: %s: %v", c, err))
		}
		t := Table{byName: map[string]Option{}}
		for _, o := range opts {
			for _, n := range o.Names {
				t.byName[n] = o
			}
		}
		loaded[c] = t
	}
}

// For returns command's table; an unknown command has an empty table.
func For(command string) Table {
	return loaded[command]
}

// Lookup finds an option token: "--name", "--name=value", "-X" or "-Xvalue".
func (t Table) Lookup(tok string) (Option, bool) {
	switch {
	case strings.HasPrefix(tok, "--"):
		name, _, _ := strings.Cut(tok, "=")
		o, ok := t.byName[name]
		return o, ok
	case len(tok) >= 2 && tok[0] == '-':
		o, ok := t.byName[tok[:2]]
		return o, ok
	}
	return Option{}, false
}

// TakesNext reports whether the option token tok takes the following token as
// its value: a known option of kind Required written without an attached value
// ("--account", "-A" but not "--account=a" or "-Aa").
func (t Table) TakesNext(tok string) bool {
	o, ok := t.Lookup(tok)
	if !ok || o.Value != Required {
		return false
	}
	if strings.HasPrefix(tok, "--") {
		return !strings.Contains(tok, "=")
	}
	return len(tok) == 2
}

// Options returns every option of the table once, in no particular order.
func (t Table) Options() []Option {
	seen := map[string]bool{}
	var out []Option
	for _, o := range t.byName {
		if !seen[o.Names[0]] {
			seen[o.Names[0]] = true
			out = append(out, o)
		}
	}
	return out
}

// ParseTable reads the fixture format: per line, the option's spellings then
// its kind; blank lines and # comments are skipped.
func ParseTable(text string) ([]Option, error) {
	var out []Option
	sc := bufio.NewScanner(strings.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return nil, fmt.Errorf("line %d: want option names and a kind: %q", n, line)
		}
		var k Kind
		switch f[len(f)-1] {
		case "none":
			k = None
		case "required":
			k = Required
		case "optional":
			k = Optional
		default:
			return nil, fmt.Errorf("line %d: unknown kind %q", n, f[len(f)-1])
		}
		out = append(out, Option{Names: f[:len(f)-1], Value: k})
	}
	return out, sc.Err()
}

// FormatTable writes opts in the fixture format.
func FormatTable(opts []Option) string {
	var b strings.Builder
	for _, o := range opts {
		b.WriteString(strings.Join(o.Names, " "))
		b.WriteString(" ")
		b.WriteString(o.Value.String())
		b.WriteString("\n")
	}
	return b.String()
}
