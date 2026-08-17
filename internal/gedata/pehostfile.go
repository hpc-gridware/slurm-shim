package gedata

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Host is one parsed, merged PE_HOSTFILE entry in first-seen order.
type Host struct {
	Name           string // short hostname
	FQDN           string // original name if it carried a domain, else Name
	Slots          int    // summed across duplicate lines (REQ-LAY-003)
	QueueInstance  string // column 3 raw, e.g. "all.q@node001"
	ClusterQueue   string // column 3 before '@', e.g. "all.q" (SI-25, feeds A14)
	ProcessorRange string // column 4, an opaque GE token (SI-25); "" if absent
}

// ParsePEHostfile parses PE_HOSTFILE content. Format (SI-25):
//
//	hostname slots queue-instance processor-range
//
// Columns 3 and 4 may be absent. Blank and whitespace-only lines are skipped;
// an empty file is an error; a line with a missing or non-numeric slot count is
// an error (SI-27). Duplicate hosts (one host granted in multiple queues) are
// merged by summing slots, keeping first-seen order and the first line's queue
// metadata (REQ-LAY-003). nodes[0] is the master (REQ-LAY-002).
func ParsePEHostfile(data []byte) ([]Host, error) {
	var hosts []Host
	index := map[string]int{} // short name -> position in hosts

	for lineNo, raw := range strings.Split(string(data), "\n") {
		fields := strings.Fields(raw)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 2 {
			return nil, fmt.Errorf("PE_HOSTFILE line %d: missing slot count: %q", lineNo+1, raw)
		}
		slots, err := strconv.Atoi(fields[1])
		if err != nil || slots < 0 {
			return nil, fmt.Errorf("PE_HOSTFILE line %d: invalid slot count %q", lineNo+1, fields[1])
		}

		short, fqdn := splitHostname(fields[0])
		if i, seen := index[short]; seen {
			hosts[i].Slots += slots
			continue
		}

		h := Host{Name: short, FQDN: fqdn, Slots: slots}
		if len(fields) >= 3 {
			h.QueueInstance = fields[2]
			if at := strings.IndexByte(fields[2], '@'); at >= 0 {
				h.ClusterQueue = fields[2][:at]
			} else {
				h.ClusterQueue = fields[2]
			}
		}
		if len(fields) >= 4 {
			h.ProcessorRange = fields[3]
		}
		index[short] = len(hosts)
		hosts = append(hosts, h)
	}

	if len(hosts) == 0 {
		return nil, errors.New("PE_HOSTFILE is empty")
	}
	return hosts, nil
}

// splitHostname returns the short name and the FQDN. When the name has no
// domain, both are the same.
func splitHostname(name string) (short, fqdn string) {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i], name
	}
	return name, name
}
