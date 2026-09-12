package sinfo

import (
	"fmt"
	"strings"
)

// options are the sinfo flags the shim honours.
//
// sinfo previously parsed nothing: every flag was silently dropped and the full
// human table printed regardless. That is worse than an error, because `-h -o`
// is precisely how scripts read sinfo --
//
//	sinfo -h -o '%P'        # enumerate partitions
//	sinfo -h -o '%n %G'     # node -> GRES, the usual way to find GPUs
//
// -- so a caller either parsed the header row as data or got nothing, with no
// indication that the flags had been ignored. squeue has honoured -h/-o since it
// was written, so the shim was also inconsistent with itself.
type options struct {
	noHeader  bool
	format    string // SLURM format string, e.g. "%P %D %T"
	formatSet bool   // -o was given, even if the value was empty
	// partitions restricts the listing. Implemented rather than rejected
	// because `sinfo -p <name>` is the most common invocation there is, and a
	// filter that is silently ignored returns a SUPERSET -- wrong data, with no
	// indication that the filter did not apply.
	partitions []string
}

// parseFlags reads the sinfo flags. Unknown flags are an ERROR rather than a
// warning: sinfo output is parsed by scripts, and silently ignoring a flag that
// changes the shape of that output is how a caller ends up reading a header row
// as data.
func parseFlags(args []string) (options, error) {
	var opt options
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, bool) {
			if i+1 < len(args) {
				i++
				return args[i], true
			}
			return "", false
		}
		switch {
		case a == "-h" || a == "--noheader":
			opt.noHeader = true
		case a == "-o" || a == "--format":
			v, ok := next()
			if !ok {
				return opt, fmt.Errorf("option %s requires an argument", a)
			}
			opt.format, opt.formatSet = v, true
		case strings.HasPrefix(a, "--format="):
			opt.format, opt.formatSet = strings.TrimPrefix(a, "--format="), true
		case a == "-p" || a == "--partition":
			v, ok := next()
			if !ok {
				return opt, fmt.Errorf("option %s requires an argument", a)
			}
			opt.partitions = append(opt.partitions, splitCSV(v)...)
		case strings.HasPrefix(a, "--partition="):
			opt.partitions = append(opt.partitions, splitCSV(strings.TrimPrefix(a, "--partition="))...)
		case strings.HasPrefix(a, "-p") && len(a) > 2:
			opt.partitions = append(opt.partitions, splitCSV(a[2:])...)
		case strings.HasPrefix(a, "-o"):
			// -o%P, the attached form
			opt.format, opt.formatSet = strings.TrimPrefix(a, "-o"), true
		default:
			return opt, fmt.Errorf("unrecognized option %q "+
				"(supported: -h/--noheader, -o/--format, -p/--partition)", a)
		}
	}
	return opt, nil
}

// field values available to a format string, per row.
type rowFields struct {
	partition string
	avail     string
	timelimit string
	nodes     string
	state     string
	nodelist  string
}

// renderFormat expands a SLURM sinfo format string for one row.
//
// Only the specifiers sinfo can actually answer from this data are supported.
// An UNKNOWN specifier is an error rather than being passed through or dropped:
// a script asking for %G (gres) and silently receiving the literal text "%G"
// would be worse than being told the field is unavailable.
//
// Width modifiers (%.10P) are accepted and ignored -- they affect padding only,
// and every consumer that matters splits on whitespace.
func renderFormat(format string, f rowFields) (string, error) {
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(format) {
			return "", fmt.Errorf("format string ends with a bare %%")
		}
		i++
		// %% is a literal percent.
		if format[i] == '%' {
			b.WriteByte('%')
			continue
		}
		// Skip a width modifier: digits, and a leading '.' for right-justify.
		if format[i] == '.' {
			i++
		}
		for i < len(format) && format[i] >= '0' && format[i] <= '9' {
			i++
		}
		if i >= len(format) {
			return "", fmt.Errorf("format string ends inside a specifier")
		}
		switch format[i] {
		case 'P':
			b.WriteString(f.partition)
		case 'a':
			b.WriteString(f.avail)
		case 'l':
			b.WriteString(f.timelimit)
		case 'D':
			b.WriteString(f.nodes)
		case 'T', 't':
			// SLURM distinguishes %T (state) from %t (abbreviated state). The
			// shim has one state vocabulary -- idle/mix/allocated/drain/down --
			// and no abbreviations to give, so both render the same string
			// rather than inventing a second form.
			b.WriteString(f.state)
		case 'N':
			b.WriteString(f.nodelist)
		default:
			return "", fmt.Errorf("unsupported format specifier %%%c "+
				"(supported: %%P partition, %%a avail, %%l timelimit, "+
				"%%D nodes, %%T state, %%N nodelist)", format[i])
		}
	}
	return b.String(), nil
}

// splitCSV splits a comma-separated flag value, dropping empties, so both
// `-p a,b` and `-p a -p b` mean the same thing.
func splitCSV(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
