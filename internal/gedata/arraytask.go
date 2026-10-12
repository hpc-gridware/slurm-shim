package gedata

import (
	"strconv"
	"strings"
)

// The shim's array contract (SI-14): a SLURM array index is the 0-based
// position of the Grid Engine task, so SLURM index k is GE task k+1. Every
// command that names an array element -- squeue, scancel, scontrol, sacct --
// converts through these two functions, so they cannot disagree.

// GETask returns the GE task number for a SLURM array index ("2" -> "3"), and
// false when the index is not a non-negative integer.
func GETask(slurmIndex string) (string, bool) {
	k, err := strconv.Atoi(slurmIndex)
	if err != nil || k < 0 {
		return "", false
	}
	return strconv.Itoa(k + 1), true
}

// SlurmTask renders a GE task id the way SLURM shows it: a single task "3" is
// index "2"; a pending range "1-10:1" is "[0-9]" and "1-9:2" is "[0-8:2]".
// Anything else is returned unchanged.
func SlurmTask(geTask string) string {
	if geTask == "" {
		return ""
	}
	if n, err := strconv.Atoi(geTask); err == nil {
		return strconv.Itoa(n - 1)
	}
	lo, hi, step, ok := parseTaskRange(geTask)
	if !ok {
		return geTask
	}
	r := strconv.Itoa(lo-1) + "-" + strconv.Itoa(hi-1)
	if step != 1 {
		r += ":" + strconv.Itoa(step)
	}
	return "[" + r + "]"
}

// TaskInRange reports whether GE task number t is one of the tasks a GE task id
// names: the task itself, or a member of a range "lo-hi:step".
func TaskInRange(geTask, t string) bool {
	if geTask == t {
		return true
	}
	n, err := strconv.Atoi(t)
	if err != nil {
		return false
	}
	lo, hi, step, ok := parseTaskRange(geTask)
	return ok && n >= lo && n <= hi && (n-lo)%step == 0
}

// parseTaskRange reads qstat's "lo-hi:step" task range.
func parseTaskRange(s string) (lo, hi, step int, ok bool) {
	span, st, hasStep := strings.Cut(s, ":")
	a, b, isRange := strings.Cut(span, "-")
	if !isRange {
		return 0, 0, 0, false
	}
	var err error
	if lo, err = strconv.Atoi(a); err != nil {
		return 0, 0, 0, false
	}
	if hi, err = strconv.Atoi(b); err != nil {
		return 0, 0, 0, false
	}
	step = 1
	if hasStep {
		if step, err = strconv.Atoi(st); err != nil || step < 1 {
			return 0, 0, 0, false
		}
	}
	return lo, hi, step, true
}
