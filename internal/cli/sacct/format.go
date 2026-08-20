package sacct

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// defaultFormat is what sacct prints when no -o/--format is given, matching
// SLURM's own default column set.
const defaultFormat = "JobID,JobName,Partition,Account,AllocCPUS,State,ExitCode"

// row is one output line's worth of job facts, assembled from qstat (live) or
// qacct (finished) before any field selection happens.
type row struct {
	jobID     string // already rendered, including any _<task> suffix
	jobName   string
	partition string
	account   string
	allocCPUS int64
	state     string
	exitCode  string
	nodeList  string
	user      string
	submit    time.Time
	start     time.Time
	end       time.Time
	elapsed   time.Duration
	maxRSS    float64
	totalCPU  time.Duration
}

// fieldAliases maps the alternate spellings SLURM accepts onto the canonical
// field name. Lookup is case-insensitive, as it is in sacct.
// Only true synonyms belong here. NNodes and UID are deliberately absent: GE's
// accounting record carries neither a node count nor a numeric uid, and aliasing
// them onto AllocCPUS/User would report a slot count as a node count and a name
// as an id -- both under the wrong column title, since header() resolves the
// alias before choosing one. They fall through to an empty column instead.
var fieldAliases = map[string]string{
	"jobidraw": "jobid",
	"job":      "jobid",
	"name":     "jobname",
	"ncpus":    "alloccpus",
	"reqcpus":  "alloccpus",
	"cpus":     "alloccpus",
	"nodes":    "nodelist",
	"exit":     "exitcode",
	"time":     "elapsed",
	"cputime":  "totalcpu",
	"username": "user",
}

// value renders one field of a row. An unknown field yields an empty column
// rather than an error: sacct tolerates fields its version does not know, and a
// hard failure here would break a caller over a cosmetic column.
func (r row) value(field string) string {
	switch canonicalField(field) {
	case "jobid":
		return r.jobID
	case "jobname":
		return r.jobName
	case "partition":
		return r.partition
	case "account":
		return r.account
	case "alloccpus":
		if r.allocCPUS <= 0 {
			return "0"
		}
		return strconv.FormatInt(r.allocCPUS, 10)
	case "state":
		return r.state
	case "exitcode":
		return r.exitCode
	case "nodelist":
		return r.nodeList
	case "user":
		return r.user
	case "submit":
		return acctTime(r.submit)
	case "start":
		return acctTime(r.start)
	case "end":
		return acctTime(r.end)
	case "elapsed":
		return acctDuration(r.elapsed)
	case "totalcpu":
		return acctDuration(r.totalCPU)
	case "maxrss":
		return acctBytes(r.maxRSS)
	default:
		return ""
	}
}

// canonicalField lowercases a requested field and resolves aliases. SLURM allows
// a display width suffix (State%20), which is accepted and ignored.
func canonicalField(field string) string {
	f := strings.ToLower(strings.TrimSpace(field))
	if i := strings.IndexByte(f, '%'); i >= 0 {
		f = f[:i]
	}
	if c, ok := fieldAliases[f]; ok {
		return c
	}
	return f
}

// header is the column title sacct prints for a field: its canonical spelling in
// SLURM's mixed case, falling back to the user's own spelling.
func header(field string) string {
	titles := map[string]string{
		"jobid": "JobID", "jobname": "JobName", "partition": "Partition",
		"account": "Account", "alloccpus": "AllocCPUS", "state": "State",
		"exitcode": "ExitCode", "nodelist": "NodeList", "user": "User",
		"submit": "Submit", "start": "Start", "end": "End",
		"elapsed": "Elapsed", "totalcpu": "TotalCPU", "maxrss": "MaxRSS",
	}
	if t, ok := titles[canonicalField(field)]; ok {
		return t
	}
	// Unknown field: echo back what was asked for, minus the width suffix, so it
	// is titled the same way a known field would be.
	f := strings.TrimSpace(field)
	if i := strings.IndexByte(f, '%'); i >= 0 {
		f = f[:i]
	}
	return f
}

// splitFields parses a --format value into field names, dropping empties so a
// trailing comma is harmless.
func splitFields(spec string) []string {
	var out []string
	for _, f := range strings.Split(spec, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// acctTime renders a timestamp the way sacct does; an unset time is "Unknown".
func acctTime(t time.Time) string {
	if t.IsZero() {
		return "Unknown"
	}
	return t.Format("2006-01-02T15:04:05")
}

// acctDuration renders a duration as sacct's [DD-]HH:MM:SS.
func acctDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int64(d / time.Second)
	days := total / 86400
	h := (total % 86400) / 3600
	m := (total % 3600) / 60
	s := total % 60
	if days > 0 {
		return fmt.Sprintf("%d-%02d:%02d:%02d", days, h, m, s)
	}
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

// acctBytes renders a byte count in sacct's K suffix form.
func acctBytes(b float64) string {
	if b <= 0 {
		return ""
	}
	return strconv.FormatInt(int64(b/1024), 10) + "K"
}

// render turns rows into output lines. parsable emits "|"-delimited fields
// (trailing delimiter only for -P, matching SLURM); otherwise it prints sacct's
// right-justified table with the dashed rule under the header.
func render(fields []string, rows []row, parsable, trailing, noHeader bool) []string {
	head := make([]string, len(fields))
	for i, f := range fields {
		head[i] = header(f)
	}
	cells := make([][]string, 0, len(rows))
	for _, r := range rows {
		line := make([]string, len(fields))
		for i, f := range fields {
			line[i] = r.value(f)
		}
		cells = append(cells, line)
	}

	if parsable {
		out := make([]string, 0, len(cells)+1)
		join := func(c []string) string {
			s := strings.Join(c, "|")
			if trailing {
				s += "|"
			}
			return s
		}
		if !noHeader {
			out = append(out, join(head))
		}
		for _, c := range cells {
			out = append(out, join(c))
		}
		return out
	}

	// Column widths always account for the header, even when it is suppressed,
	// so -n output stays aligned with the same command's headed output. Widths
	// are counted in runes, not bytes: a job name with any multi-byte character
	// would otherwise widen its column and shift every other row out of line.
	widths := make([]int, len(fields))
	for i, h := range head {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, c := range cells {
		for i, v := range c {
			if n := utf8.RuneCountInString(v); n > widths[i] {
				widths[i] = n
			}
		}
	}
	pad := func(c []string) string {
		parts := make([]string, len(c))
		for j, v := range c {
			parts[j] = strings.Repeat(" ", widths[j]-utf8.RuneCountInString(v)) + v
		}
		return strings.Join(parts, " ")
	}

	out := make([]string, 0, len(cells)+2)
	if !noHeader {
		rule := make([]string, len(fields))
		for i := range fields {
			rule[i] = strings.Repeat("-", widths[i])
		}
		out = append(out, pad(head), strings.Join(rule, " "))
	}
	for _, c := range cells {
		out = append(out, pad(c))
	}
	return out
}
