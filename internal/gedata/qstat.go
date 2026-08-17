package gedata

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
)

// qstat -xml -j request subtree, minimal to the hard resource requests. A job
// carries one request set per scope (global is scope 0); each hard resource is a
// qstat_l_requests element with the complex name and its parsed numeric value
// (CE_doubleval holds bytes for a MEMORY-typed complex).
type requestJobXML struct {
	Jobs []struct {
		Sets []struct {
			Hard []ceRequestXML `xml:"JRS_hard_resource_list>qstat_l_requests"`
		} `xml:"JB_request_set_list>ulong_sublist"`
	} `xml:"djob_info>element"`
}

type ceRequestXML struct {
	Name      string  `xml:"CE_name"`
	DoubleVal float64 `xml:"CE_doubleval"`
}

// ParseRequestedResourceXML returns the requested amount of the named complex
// from `qstat -xml -j` output - CE_doubleval, which is bytes for a MEMORY-typed
// complex such as h_vmem. ok is false when the job did not request the complex,
// letting callers distinguish "not requested" from a requested value of zero.
func ParseRequestedResourceXML(data []byte, complexName string) (amount float64, ok bool, err error) {
	var doc requestJobXML
	if err := xml.Unmarshal(data, &doc); err != nil {
		return 0, false, fmt.Errorf("parse qstat -xml -j: %w", err)
	}
	for _, job := range doc.Jobs {
		for _, set := range job.Sets {
			for _, req := range set.Hard {
				if req.Name == complexName {
					return req.DoubleVal, true, nil
				}
			}
		}
	}
	return 0, false, nil
}

// RequestedResource runs `qstat -xml -j <jobID>` and returns the requested
// amount (CE_doubleval) of complexName. It is the source for A27 memory
// (REQ-FAB-003): the caller supplies a timeout-bounded context and treats a
// non-nil error as "skip with a warning". ok is false when the complex was not
// requested.
func RequestedResource(ctx context.Context, r Runner, jobID, complexName string) (float64, bool, error) {
	stdout, stderr, exit, err := r.Run(ctx, "qstat", "-xml", "-j", jobID)
	if err != nil {
		return 0, false, fmt.Errorf("qstat -xml -j %s: %w", jobID, err)
	}
	if exit != 0 {
		return 0, false, fmt.Errorf("qstat -xml -j %s: exit %d: %s", jobID, exit, strings.TrimSpace(string(stderr)))
	}
	return ParseRequestedResourceXML(stdout, complexName)
}
