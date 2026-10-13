package squeue

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/config"
	"github.com/hpc-gridware/slurm-shim/internal/encoders"
	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// token is one piece of a parsed format: a literal run, or a field verb with a
// field size, its justification, and (for -O) a suffix printed after it.
type token struct {
	literal string
	isField bool
	verb    byte
	width   int
	right   bool
	suffix  string
}

// tokenize parses a squeue -o format string. A field is "%[.]<size><verb>":
// "." right-justifies, size is the field size; "%%" is a literal percent; other
// characters pass through (REQ-SQU-002).
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
		right := false
		if format[i] == '.' {
			right = true
			i++
		}
		width := 0
		for i < len(format) && format[i] >= '0' && format[i] <= '9' {
			width = width*10 + int(format[i]-'0')
			i++
		}
		if i < len(format) {
			toks = append(toks, token{isField: true, width: width, right: right, verb: format[i]})
			i++
		}
	}
	return toks
}

// longFields maps the squeue -O field names the shim renders (matched without
// regard to case) to the -o verb that renders the same value.
var longFields = map[string]byte{
	"jobid":         'i',
	"partition":     'P',
	"name":          'j',
	"username":      'u',
	"state":         'T',
	"statecompact":  't',
	"timeused":      'M',
	"timelimit":     'l',
	"numnodes":      'D',
	"nodelist":      'N',
	"reason":        'r',
	"starttime":     'S',
	"submittime":    'V',
	"account":       'a',
	"priority":      'p',
	"numcpus":       'C',
	"minmemory":     'm',
	"workdir":       'Z',
	"tres-per-node": 'b',
}

// longFieldSize is the size SLURM gives a -O field that names none.
const longFieldSize = 20

// tokenizeLong parses a squeue -O format: comma-separated
// "type[:[.][size][suffix]]" fields, left-justified in 20 characters unless a
// size is given. Fields the shim does not render are returned by name so the
// caller can warn about them; they are left out of the output.
func tokenizeLong(format string) (toks []token, unknown []string) {
	for _, field := range strings.Split(format, ",") {
		name, spec, _ := strings.Cut(strings.TrimSpace(field), ":")
		if name == "" {
			continue
		}
		verb, ok := longFields[strings.ToLower(name)]
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		t := token{isField: true, verb: verb, width: longFieldSize}
		if strings.HasPrefix(spec, ".") {
			t.right = true
			spec = spec[1:]
		}
		digits := 0
		for digits < len(spec) && spec[digits] >= '0' && spec[digits] <= '9' {
			digits++
		}
		if digits > 0 {
			t.width, _ = strconv.Atoi(spec[:digits])
		}
		t.suffix = spec[digits:]
		toks = append(toks, t)
	}
	return toks, unknown
}

func render(toks []token, value func(byte) string) string {
	var b strings.Builder
	for _, t := range toks {
		if !t.isField {
			b.WriteString(t.literal)
			continue
		}
		b.WriteString(fit(value(t.verb), t.width, t.right))
		b.WriteString(t.suffix)
	}
	return b.String()
}

// fit sizes a value to its field the way squeue does: a sized field is cut to
// its size (this is why SLURM's default format shows only 8 characters of a job
// name) and padded on the side opposite its justification.
func fit(v string, width int, right bool) string {
	if width <= 0 {
		return v
	}
	if len(v) > width {
		v = v[:width]
	}
	if right {
		return fmt.Sprintf("%*s", width, v)
	}
	return fmt.Sprintf("%-*s", width, v)
}

func formatHeader(toks []token) string {
	return render(toks, headerTitle)
}

// view is everything a row needs beyond the row itself: the config, the granted
// host lists keyed by gedata.JobKey, each job's `qstat -xml -j` detail keyed by
// job id, and one clock reading shared by every row so a long listing cannot
// show two different elapsed times for the same instant.
type view struct {
	cfg     *config.Config
	hosts   map[string]gedata.JobAllocation
	details map[string]gedata.JobInfo
	// schedInfo says the scheduler records why jobs wait (schedd_job_info).
	schedInfo bool
	// holdKinds names the hold of held jobs whose detail does not (OCS 9.0,
	// see gedata.HoldKinds); nil when no listed job needed it.
	holdKinds map[string]string
	now       time.Time
}

// unexplainedHold reports a job whose state shows a hold its detail does not
// account for: no hold kind, no unfinished predecessor.
func (v view) unexplainedHold(row gedata.JobRow) bool {
	d, ok := v.details[row.JobID]
	return ok && strings.ContainsRune(row.State, 'h') && !strings.ContainsRune(row.State, 'E') &&
		d.Hold(row.TaskID) == gedata.HoldNone && len(d.Predecessors) == 0
}

func (v view) formatRow(toks []token, row gedata.JobRow) string {
	return render(toks, func(b byte) string { return v.rowValue(b, row) })
}

// hostVerbs are the format verbs whose value names the granted hosts. A row
// whose hosts had to be guessed is reported on stderr when one of them is shown.
var hostVerbs = []byte{'D', 'N', 'R'}

// detailVerbs are the format verbs whose value needs the `qstat -xml -j` query:
// the granted hosts, and every field plain qstat -xml does not carry. needsDetail
// gates the query on this set and rowValue reads the detail for exactly these
// verbs; squeue_test.go asserts the two agree, so a new column cannot be added
// to one without the other and end up always empty.
var detailVerbs = []byte{'D', 'N', 'R', 'P', 'r', 'l', 'S', 'V', 'a', 'c', 'm', 'b', 'Z'}

func usesVerb(toks []token, verbs []byte) bool {
	for _, t := range toks {
		if t.isField && bytes.IndexByte(verbs, t.verb) >= 0 {
			return true
		}
	}
	return false
}

// needsDetail reports whether a format asks for a column that requires the
// detail query, so squeue can skip it when it does not.
func needsDetail(toks []token) bool { return usesVerb(toks, detailVerbs) }

// needsHosts reports whether a format shows a node column.
func needsHosts(toks []token) bool { return usesVerb(toks, hostVerbs) }

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
	if h := v.hosts[row.Key()].Hosts; len(h) > 0 {
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
	return started(row) && len(v.hosts[row.Key()].Hosts) == 0
}

// headers are SLURM 24.05's column titles per verb.
var headers = map[byte]string{
	'i': "JOBID",
	'P': "PARTITION",
	'j': "NAME",
	'u': "USER",
	't': "ST",
	'T': "STATE",
	'M': "TIME",
	'D': "NODES",
	'R': "NODELIST(REASON)",
	'C': "CPUS",
	'N': "NODELIST",
	'r': "REASON",
	'l': "TIME_LIMIT",
	'S': "START_TIME",
	'V': "SUBMIT_TIME",
	'a': "ACCOUNT",
	'p': "PRIORITY",
	'Q': "PRIORITY",
	'c': "MIN_CPUS",
	'm': "MIN_MEMORY",
	'b': "TRES_PER_NODE",
	'Z': "WORK_DIR",
}

func headerTitle(verb byte) string { return headers[verb] }

func (v view) rowValue(verb byte, row gedata.JobRow) string {
	d, known := v.details[row.JobID]
	switch verb {
	case 'i':
		return slurmID(row)
	case 'P':
		return v.partition(row)
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
			return "(" + v.reason(row) + ")"
		}
		return nodelist(v.hostsFor(row))
	case 'r':
		if started(row) {
			return "None"
		}
		return v.reason(row)
	case 'C':
		return strconv.Itoa(row.Slots)
	case 'c':
		nodes := len(v.hostsFor(row))
		if nodes < 1 {
			nodes = 1
		}
		return strconv.Itoa((row.Slots + nodes - 1) / nodes)
	case 'N':
		return nodelist(v.hostsFor(row))
	case 'l':
		if !known {
			return "N/A"
		}
		if limit, ok := d.Requests["h_rt"]; ok && limit.Amount > 0 {
			return slurmDuration(time.Duration(limit.Amount) * time.Second)
		}
		return "UNLIMITED"
	case 'S':
		if started(row) && !row.Start.IsZero() {
			return slurmTime(row.Start)
		}
		// SLURM shows a pending job's expected start; the one Grid Engine knows
		// is the -a time it may not start before.
		return slurmTime(d.Begin)
	case 'V':
		if !row.Submit.IsZero() {
			return slurmTime(row.Submit)
		}
		return slurmTime(d.Submit)
	case 'a':
		return orNull(d.Account)
	case 'm':
		req, ok := d.Requests[v.cfg.MemoryComplex]
		if !ok || req.Amount <= 0 {
			return "0"
		}
		return slurmMemory(req.Amount)
	case 'b':
		req, ok := d.Requests[v.cfg.GPU.GresComplex]
		if !ok || req.Amount <= 0 {
			return "N/A"
		}
		return "gres/gpu:" + strconv.FormatFloat(req.Amount, 'f', -1, 64)
	case 'Z':
		return orNull(d.WorkDir)
	case 'p':
		return strconv.FormatFloat(row.Prio, 'f', 8, 64)
	case 'Q':
		// SLURM's %p is its integer priority divided by 2^32-1; this is the
		// inverse, so the two columns agree and %Q sorts as Grid Engine ranks.
		return strconv.FormatUint(uint64(math.Round(row.Prio*math.MaxUint32)), 10)
	default:
		return ""
	}
}

// Pending reasons, in SLURM's spelling.
const (
	reasonNone      = "None"
	reasonFailed    = "launch failed requeued held"
	reasonHeldUser  = "JobHeldUser"
	reasonHeldAdmin = "JobHeldAdmin"
	reasonDepend    = "Dependency"
	reasonBegin     = "BeginTime"
	reasonResources = "Resources"
	reasonPriority  = "Priority"
)

// reason is why a pending job is not running. The job's own state answers first
// and needs no scheduler setting: an error state (Eqw, a job Grid Engine failed
// to start and holds until it is cleared), a hold, an unfinished predecessor, a
// start time still in the future. Only then is the scheduler asked, and it can answer
// only when schedd_job_info is enabled: a job it left a message for is short of
// resources, one it did not is waiting its turn. Without that setting there is
// nothing more to say, which SLURM spells None.
func (v view) reason(row gedata.JobRow) string {
	if strings.ContainsRune(row.State, 'E') {
		return reasonFailed
	}
	d, ok := v.details[row.JobID]
	if !ok {
		return reasonNone
	}
	switch d.Hold(row.TaskID) {
	case gedata.HoldUser:
		return reasonHeldUser
	case gedata.HoldAdmin:
		return reasonHeldAdmin
	case gedata.HoldDependency:
		return reasonDepend
	}
	if v.unexplainedHold(row) {
		switch v.holdKinds[row.JobID] {
		case gedata.HoldUser:
			return reasonHeldUser
		case gedata.HoldAdmin:
			return reasonHeldAdmin
		}
	}
	switch {
	case len(d.Predecessors) > 0:
		return reasonDepend
	case d.Begin.After(v.now):
		return reasonBegin
	case len(d.SchedulerMessages) > 0:
		return reasonResources
	case v.schedInfo:
		return reasonPriority
	}
	return reasonNone
}

// partition names the SLURM partition a job belongs to, from the master queue
// instance of a started job, else the queue it requested, and its PE.
func (v view) partition(row gedata.JobRow) string {
	queue := row.Queue
	d := v.details[row.JobID]
	if queue == "" && len(d.Queues) > 0 {
		queue = d.Queues[0]
	}
	return v.cfg.PartitionFor(queue, d.PE)
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
// 0:00 until a job starts.
func squeueElapsed(row gedata.JobRow, now time.Time) string {
	if row.Start.IsZero() || !started(row) {
		return "0:00"
	}
	return slurmDuration(now.Sub(row.Start))
}

// slurmDuration renders a duration as squeue does for TIME and TIME_LIMIT,
// which is more compact than sacct's: no leading zero on the hour, and no hour
// at all below one.
func slurmDuration(d time.Duration) string {
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

// slurmTime renders a timestamp as squeue does, "N/A" when there is none.
func slurmTime(t time.Time) string {
	if t.IsZero() {
		return "N/A"
	}
	return t.Format("2006-01-02T15:04:05")
}

// slurmMemory renders a byte count as squeue's MIN_MEMORY does: megabytes, with
// the largest unit that divides it exactly.
func slurmMemory(bytes float64) string {
	mb := int64(math.Round(bytes / (1 << 20)))
	switch {
	case mb > 0 && mb%(1<<20) == 0:
		return strconv.FormatInt(mb>>20, 10) + "T"
	case mb > 0 && mb%(1<<10) == 0:
		return strconv.FormatInt(mb>>10, 10) + "G"
	default:
		return strconv.FormatInt(mb, 10) + "M"
	}
}

func orNull(s string) string {
	if s == "" {
		return "(null)"
	}
	return s
}

// hostOf returns the host part of a "queue@host" instance.
func hostOf(queue string) string {
	if at := strings.IndexByte(queue, '@'); at >= 0 {
		return queue[at+1:]
	}
	return ""
}

// slurmID is the job id as SLURM shows it: an array element by its 0-based
// index ("4711_0"), pending tasks as "4711_[0-9]" or "4711_[0,3]" (SI-14).
func slurmID(row gedata.JobRow) string {
	return gedata.JobKey(row.JobID, gedata.SlurmTaskList(row.TaskID))
}
