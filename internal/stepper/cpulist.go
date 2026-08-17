package stepper

import (
	"fmt"
	"strconv"
	"strings"
)

// parseCPUList parses a SLURM/GE cpuset string like "0-3,5" into CPU ids.
func parseCPUList(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if dash := strings.IndexByte(part, '-'); dash >= 0 {
			lo, err1 := strconv.Atoi(part[:dash])
			hi, err2 := strconv.Atoi(part[dash+1:])
			if err1 != nil || err2 != nil || hi < lo {
				return nil, fmt.Errorf("invalid cpu range %q", part)
			}
			for c := lo; c <= hi; c++ {
				out = append(out, c)
			}
			continue
		}
		c, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid cpu %q", part)
		}
		out = append(out, c)
	}
	return out, nil
}
