package gedata

import (
	"strconv"
	"strings"
)

// qstat reports the pending tasks of an array as a task list: one task ("3"), a
// range ("1-5:1"), or, once some tasks have left the range (released, started),
// a comma list of either ("1,4", "1-3:1,7"). arraytask.go handles the first two
// forms; these helpers accept all three.

// ExpandTasks lists the GE task numbers a task list names, in order. A part that
// is not a task number or range is returned as is.
func ExpandTasks(list string) []string {
	var out []string
	for _, part := range strings.Split(list, ",") {
		lo, hi, step, ok := parseTaskRange(part)
		if !ok {
			out = append(out, part)
			continue
		}
		for t := lo; t <= hi; t += step {
			out = append(out, strconv.Itoa(t))
		}
	}
	return out
}

// TaskListContains reports whether GE task number t is one of the tasks a task
// list names.
func TaskListContains(list, t string) bool {
	for _, part := range strings.Split(list, ",") {
		if TaskInRange(part, t) {
			return true
		}
	}
	return false
}

// IsTaskList reports whether a GE task id names more than one task: a range or
// a comma list.
func IsTaskList(geTask string) bool {
	return strings.ContainsAny(geTask, "-,")
}

// SlurmTaskList renders a GE task list the way SLURM shows a pending array:
// "1-10:1" is "[0-9]", "1,4" is "[0,3]", "1-3:1,7" is "[0-2,6]". A single task
// is its 0-based index, as SlurmTask renders it.
func SlurmTaskList(list string) string {
	if !strings.Contains(list, ",") {
		return SlurmTask(list)
	}
	parts := strings.Split(list, ",")
	for i, part := range parts {
		parts[i] = strings.Trim(SlurmTask(part), "[]")
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// firstTask is the GE task a task id names first: the number itself, the low
// end of a range or list, or 1 for a job that is not an array.
func firstTask(geTask string) int {
	first, _, _ := strings.Cut(geTask, ",")
	if n, err := strconv.Atoi(first); err == nil {
		return n
	}
	if lo, _, _, ok := parseTaskRange(first); ok {
		return lo
	}
	return 1
}
