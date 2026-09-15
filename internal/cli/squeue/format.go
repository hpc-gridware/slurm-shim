package squeue

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/encoders"
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

// view is everything a row needs beyond the row itself: the config, the granted
// host lists keyed by gedata.JobKey, and one clock reading shared by every row so
// a long listing cannot show two different elapsed times for the same instant.
// It is a struct rather than more positional parameters because every column
// datum added so far has widened both the renderer and its call sites.
type view struct {
	cfg   *config.Config
	hosts map[string][]string
	now   time.Time
}

func (v view) formatRow(format string, row gedata.JobRow) string {
	return render(tokenize(format), func(b byte) string { return v.rowValue(b, row) })
}

// hostVerbs are the format verbs whose value comes from the granted host list.
// needsHosts gates the supplementary query on this set and rowValue reads the
// hosts for exactly these verbs; format_test.go asserts the two agree, so a new
// node column cannot be added to one without the other and end up always empty.
var hostVerbs = []byte{'D', 'N', 'R'}

// needsHosts reports whether a format asks for a column that requires the node
// list, so squeue can skip the supplementary qstat when it does not.
func needsHosts(format string) bool {
	for _, t := range tokenize(format) {
		if t.isField && bytes.IndexByte(hostVerbs, t.verb) >= 0 {
			return true
		}
	}
	return false
}

// started reports whether a job holds an allocation and has a running clock.
// Grid Engine records a start time, and keeps the granted hosts, for states SLURM
// calls SUSPENDED and COMPLETING as well as RUNNING -- and a site that implements
// preemption with subordinate queues suspends jobs as a matter of routine, so
// gating on RUNNING alone would make the normal appearance of a normal cluster
// read as "never started, nowhere".
func started(row gedata.JobRow) bool {
	switch gedata.MapState(row.State) {
	case "R", "S", "CG":
		return true
	}
	return false
}

// hostsFor returns the hosts to render for a row. A job that has not started has
// no allocation and gets none. For a started job whose hosts are missing -- the
// supplementary query failed, or the job started between the two queries -- it
// falls back to the master queue instance, which is one host the job really is
// on. That single host is then the source for every node column of the row, so
// the count and the list cannot contradict each other; degraded says when it
// happened, and squeue reports it.
func (v view) hostsFor(row gedata.JobRow) []string {
	if h := v.hosts[row.Key()]; len(h) > 0 {
		return h
	}
	if !started(row) {
		return nil
	}
	if host := hostOf(row.Queue); host != "" {
		return []string{host}
	}
	return nil
}

// degraded reports whether a started row's allocation had to be guessed because
// the host map carried nothing for it. The guess is indistinguishable in stdout
// from a correct single-node answer, so the caller warns on stderr instead of
// letting it pass as fact.
func (v view) degraded(row gedata.JobRow) bool {
	return started(row) && len(v.hosts[row.Key()]) == 0
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

func (v view) rowValue(verb byte, row gedata.JobRow) string {
	switch verb {
	case 'i':
		return row.Key()
	case 'P':
		return partition(row.Queue, v.cfg)
	case 'j':
		return row.Name
	case 'u':
		return row.User
	case 't':
		return gedata.MapState(row.State)
	case 'T':
		return gedata.FullState(gedata.MapState(row.State))
	case 'M':
		return squeueElapsed(row, v.now)
	case 'D':
		if hosts := v.hostsFor(row); len(hosts) > 0 {
			return strconv.Itoa(len(hosts))
		}
		// A job that has not started holds no nodes. SLURM shows the node count it
		// was asked for; the shim pins that count at submit only on clusters that
		// support it, and qstat does not report it back, so 1 is the honest floor
		// rather than a derived figure.
		return "1"
	case 'R':
		if !started(row) {
			// SLURM puts the scheduler's reason here for a job that is not running.
			// Grid Engine's reason lives in a separate query this command does not
			// make, so the column says only that there is no allocation to name.
			return "(None)"
		}
		return nodelist(v.hostsFor(row))
	case 'C':
		return strconv.Itoa(row.Slots)
	case 'N':
		return nodelist(v.hostsFor(row))
	default:
		return ""
	}
}

// nodelist renders the hosts holding a job, compressed the way
// SLURM_JOB_NODELIST is. The order is the scheduler's grant order and is never
// sorted: REQ-ENC-002 as amended by SI-41 removed the sort option precisely
// because a sorted encoding desynchronises a derived master address from rank-0
// placement. The slice belongs to the caller's map, so this must not reorder it
// in place either.
func nodelist(hosts []string) string {
	if len(hosts) == 0 {
		return ""
	}
	return encoders.CompressNodelist(hosts)
}

// squeueElapsed is the TIME column: how long a job has been running. SLURM shows
// 0:00 until a job starts, and squeue's own format is more compact than sacct's
// (no leading zero on the hour, and no hour at all below one).
func squeueElapsed(row gedata.JobRow, now time.Time) string {
	if row.Start.IsZero() || !started(row) {
		return "0:00"
	}
	d := now.Sub(row.Start)
	if d < 0 {
		d = 0
	}
	total := int64(d / time.Second)
	days := total / 86400
	h := (total % 86400) / 3600
	m := (total % 3600) / 60
	sec := total % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%d-%02d:%02d:%02d", days, h, m, sec)
	case h > 0:
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	default:
		return fmt.Sprintf("%d:%02d", m, sec)
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
