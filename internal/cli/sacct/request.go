package sacct

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/hpc-gridware/slurm-shim/internal/gedata"
)

// jobRequest is what a finished job asked for, recovered from the qsub command
// line qacct records (submit_cmd_line).
//
// That line is the only source there is. qacct 9.1.6 prints neither the job's
// resource category nor its working directory (the category is in the
// accounting file, but qacct does not show it), so requests that did not come
// from the command line -- `#$` directives inside the script, sge_request
// defaults, queue limits -- are invisible here. Every shim-submitted job
// carries its requests on the command line, which is what makes this useful.
type jobRequest struct {
	timeLimit    time.Duration // h_rt
	hasTimeLimit bool
	unlimited    bool   // h_rt=INFINITY
	mem          string // the memory complex's raw GE value, e.g. "4G"
	workDir      string // -wd; -cwd names no directory and leaves it empty
	perHost      string // -par: an integer or a rule such as $pe_slots
}

// qsubFlags are the qsub/qrsh options that take no value. Every other option
// takes one, except -pe, which takes two. Knowing this is what lets the parse
// stop at the job's own command: the arguments of `qsub -b y prog -l x` belong
// to prog, not to qsub.
var qsubFlags = map[string]bool{
	"-clear": true, "-cwd": true, "-h": true, "-hard": true, "-help": true,
	"-notify": true, "-soft": true, "-terse": true, "-verify": true, "-V": true,
	"-inherit": true, "-nostdin": true, "-noshell": true, "-verbose": true,
}

// requestOptions are the qsub options whose values make up the request read
// here. Seen after what looked like the job's command, one of them means the
// walk stopped early (see parseSubmitLine).
var requestOptions = map[string]bool{
	"-l": true, "-wd": true, "-cwd": true, "-par": true, "-scope": true, "-soft": true, "-hard": true,
}

// parseSubmitLine extracts the request from a recorded qsub command line. Only
// hard requests in the global scope count: a -soft request is a preference and
// a master/slave scoped one does not bound the job.
//
// The line is GE's argv joined by spaces, so a value that contained a space
// (-N 'my run', -wd '/data/my dir') splits into tokens and the second one looks
// like the job's command. When a request option still follows that point, the
// walk cannot tell whether it is qsub's or the job's own argument, so the
// whole request is unknown: printing nothing beats a limit read wrongly.
func parseSubmitLine(line, memComplex string) jobRequest {
	tokens := strings.Fields(line)
	var req jobRequest
	if len(tokens) == 0 {
		return req
	}
	soft, scoped := false, false
	for i := 1; i < len(tokens); i++ {
		tok := tokens[i]
		if !strings.HasPrefix(tok, "-") {
			// The job's command: everything after it is its own.
			for _, rest := range tokens[i+1:] {
				if requestOptions[rest] {
					return jobRequest{}
				}
			}
			break
		}
		if qsubFlags[tok] {
			switch tok {
			case "-soft":
				soft = true
			case "-hard":
				soft = false
			case "-cwd":
				req.workDir = ""
			}
			continue
		}
		if i+1 >= len(tokens) {
			break
		}
		val := tokens[i+1]
		i++
		switch tok {
		case "-pe":
			i++ // -pe <name> <slots>
		case "-scope":
			scoped = val != "global"
		case "-wd":
			req.workDir = val
		case "-par":
			req.perHost = val
		case "-l":
			if !soft && !scoped {
				req.applyResources(val, memComplex)
			}
		}
	}
	return req
}

// applyResources reads one -l list; a later request for the same complex
// replaces an earlier one, as it does in qsub.
func (req *jobRequest) applyResources(list, memComplex string) {
	for _, kv := range strings.Split(list, ",") {
		name, val, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch name {
		case "h_rt":
			secs, unlimited, ok := gedata.ParseTime(val)
			req.timeLimit, req.unlimited, req.hasTimeLimit = time.Duration(secs)*time.Second, unlimited, ok && !unlimited
		case memComplex:
			if memComplex != "" {
				req.mem = val
			}
		}
	}
}

// timelimit renders SLURM's Timelimit: the requested h_rt, UNLIMITED for
// h_rt=INFINITY, and "" otherwise. A command line without h_rt is not proof of
// an unlimited job: an `#$` line, sge_request or the queue may set one.
func (req jobRequest) timelimit() string {
	switch {
	case req.unlimited:
		return "UNLIMITED"
	case req.hasTimeLimit:
		return acctDuration(req.timeLimit)
	}
	return ""
}

// reqMem renders the memory request in SLURM's ReqMem form: megabytes, raised
// to G/T/P only while the value divides exactly ("4096M" prints as "4G",
// "1500M" stays "1500M"). A value GE would not accept prints nothing.
func (req jobRequest) reqMem() string {
	b, ok := gedata.MemBytes(req.mem)
	if !ok || b <= 0 {
		return ""
	}
	n := int64(math.Ceil(b / (1 << 20)))
	units := []string{"M", "G", "T", "P"}
	u := 0
	for u < len(units)-1 && n%1024 == 0 {
		n /= 1024
		u++
	}
	return strconv.FormatInt(n, 10) + units[u]
}
