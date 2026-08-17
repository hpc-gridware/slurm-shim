package encoders

import (
	"fmt"
	"strconv"
	"strings"
)

// CompressCounts run-length-encodes per-node task counts in nodelist order into
// SLURM's SLURM_TASKS_PER_NODE form (Encoder N2, spec section 8.5): a run of the
// value v with length k emits "v" when k == 1 and "v(xk)" when k > 1, comma
// joined with no spaces. E.g. [8,8,8,4] -> "8(x3),4".
func CompressCounts(counts []int) string {
	var parts []string
	for i := 0; i < len(counts); {
		j := i
		for j < len(counts) && counts[j] == counts[i] {
			j++
		}
		if run := j - i; run == 1 {
			parts = append(parts, strconv.Itoa(counts[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d(x%d)", counts[i], run))
		}
		i = j
	}
	return strings.Join(parts, ",")
}

// ExpandCounts is the inverse of CompressCounts. The fabricator uses it to
// self-check that the sum of the encoded per-node counts equals SLURM_NTASKS
// before export (REQ-FAB-006).
func ExpandCounts(s string) ([]int, error) {
	if s == "" {
		return nil, ErrInvalidHostlist{"empty tasks-per-node"}
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		value, reps, err := parseRun(part)
		if err != nil {
			return nil, err
		}
		for r := 0; r < reps; r++ {
			out = append(out, value)
		}
	}
	return out, nil
}

// parseRun parses one RLE term, "v" or "v(xk)".
func parseRun(part string) (value, reps int, err error) {
	open := strings.IndexByte(part, '(')
	if open < 0 {
		v, e := strconv.Atoi(part)
		if e != nil {
			return 0, 0, ErrInvalidHostlist{"bad count " + part}
		}
		return v, 1, nil
	}
	if !strings.HasSuffix(part, ")") || !strings.HasPrefix(part[open:], "(x") {
		return 0, 0, ErrInvalidHostlist{"bad run " + part}
	}
	v, e := strconv.Atoi(part[:open])
	if e != nil {
		return 0, 0, ErrInvalidHostlist{"bad count " + part}
	}
	k, e := strconv.Atoi(part[open+2 : len(part)-1])
	if e != nil || k < 1 {
		return 0, 0, ErrInvalidHostlist{"bad repeat " + part}
	}
	return v, k, nil
}
