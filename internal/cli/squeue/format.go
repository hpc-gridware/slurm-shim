package squeue

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// token is one piece of a parsed format string: a literal run or a field verb
// with an optional right-justify width.
type token struct {
	literal string
	width   int
	verb    byte
	isField bool
}

// tokenize parses a squeue format string. A field is "%[.]<width><verb>"; "%%"
// is a literal percent; other characters pass through (REQ-SQU-002).
func tokenize(format string) []token {
	var toks []token
	for i := 0; i < len(format); {
		if format[i] != '%' || i+1 >= len(format) {
			j := i + 1
			for j < len(format) && format[j] != '%' {
				j++
			}
			toks = append(toks, token{literal: format[i:j]})
			i = j
			continue
		}
		i++ // consume '%'
		if format[i] == '%' {
			toks = append(toks, token{literal: "%"})
			i++
			continue
		}
		if format[i] == '.' {
			i++
		}
		width := 0
		for i < len(format) && format[i] >= '0' && format[i] <= '9' {
			width = width*10 + int(format[i]-'0')
			i++
		}
		if i < len(format) {
			toks = append(toks, token{isField: true, width: width, verb: format[i]})
			i++
		}
	}
	return toks
}

func render(toks []token, value func(byte) string) string {
	var b strings.Builder
	for _, t := range toks {
		if !t.isField {
			b.WriteString(t.literal)
			continue
		}
		v := value(t.verb)
		if t.width > 0 {
			fmt.Fprintf(&b, "%*s", t.width, v)
		} else {
			b.WriteString(v)
		}
	}
	return b.String()
}

func formatHeader(format string) string {
	return render(tokenize(format), headerTitle)
}

func formatRow(format string, row gedata.JobRow, cfg *config.Config) string {
	return render(tokenize(format), func(v byte) string { return rowValue(v, row, cfg) })
}

func headerTitle(verb byte) string {
	switch verb {
	case 'i':
		return "JOBID"
	case 'P':
		return "PARTITION"
	case 'j':
		return "NAME"
	case 'u':
		return "USER"
	case 't':
		return "ST"
	case 'T':
		return "STATE"
	case 'M':
		return "TIME"
	case 'D':
		return "NODES"
	case 'R':
		return "NODELIST(REASON)"
	case 'C':
		return "CPUS"
	case 'N':
		return "NODELIST"
	default:
		return ""
	}
}

func rowValue(verb byte, row gedata.JobRow, cfg *config.Config) string {
	switch verb {
	case 'i':
		if row.TaskID != "" {
			return row.JobID + "_" + row.TaskID
		}
		return row.JobID
	case 'P':
		return partition(row.Queue, cfg)
	case 'j':
		return row.Name
	case 'u':
		return row.User
	case 't':
		return gedata.MapState(row.State)
	case 'T':
		return gedata.FullState(gedata.MapState(row.State))
	case 'M':
		return "0:00" // elapsed time is not carried in qstat -xml; placeholder
	case 'D':
		return "1" // node count requires a GE query the shim does not make here
	case 'R':
		if gedata.MapState(row.State) == "R" {
			return hostOf(row.Queue)
		}
		return "(None)"
	case 'C':
		return strconv.Itoa(row.Slots)
	case 'N':
		return hostOf(row.Queue)
	default:
		return ""
	}
}

// partition maps a queue instance to a SLURM partition via the aliases map.
func partition(queue string, cfg *config.Config) string {
	cluster := queue
	if at := strings.IndexByte(queue, '@'); at >= 0 {
		cluster = queue[:at]
	}
	if alias, ok := cfg.PartitionAliases[cluster]; ok {
		return alias
	}
	return cluster
}

// hostOf returns the host part of a "queue@host" instance.
func hostOf(queue string) string {
	if at := strings.IndexByte(queue, '@'); at >= 0 {
		return queue[at+1:]
	}
	return ""
}
