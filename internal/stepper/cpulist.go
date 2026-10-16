/*___INFO__MARK_BEGIN__*/
/*************************************************************************
*  Copyright 2026 HPC-Gridware GmbH
*
*  Licensed under the Apache License, Version 2.0 (the "License");
*  you may not use this file except in compliance with the License.
*  You may obtain a copy of the License at
*
*      http://www.apache.org/licenses/LICENSE-2.0
*
*  Unless required by applicable law or agreed to in writing, software
*  distributed under the License is distributed on an "AS IS" BASIS,
*  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
*  See the License for the specific language governing permissions and
*  limitations under the License.
*
************************************************************************/
/*___INFO__MARK_END__*/

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
