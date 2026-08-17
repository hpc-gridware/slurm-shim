package gedata

import (
	"context"
	"encoding/xml"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// HostGPUs is the set of GPU device indices granted to a job on one exec host.
// Indices are physical device numbers (REQ-GPU-002): the shim partitions them
// among the node's local ranks to build each rank's visible-device set.
type HostGPUs struct {
	Host    string
	Devices []int
}

// GrantedGPUs returns the GPUs granted to a job, keyed by exec host, using
// `qstat -xml -j <jobID>` as the source (REQ-GPU-001, SI-19). The XML detailed
// view is host-qualified, so it is correct for multi-host jobs; the
// SGE_HGR_<complex> environment variable is not (each host's execd reports only
// its own devices - a known last-wins bug). complexName is the RSMAP complex
// name (config gpu.gres_complex, e.g. "gpu").
//
// A job with no granted GPUs returns an empty slice and no error. A non-zero
// qstat exit is returned as an error so callers can apply REQ-GPU-001's
// "non-fatal unless task_policy: gpu" rule.
func GrantedGPUs(ctx context.Context, r Runner, jobID, complexName string) ([]HostGPUs, error) {
	stdout, stderr, exit, err := r.Run(ctx, "qstat", "-xml", "-j", jobID)
	if err != nil {
		return nil, fmt.Errorf("qstat -xml -j %s: %w", jobID, err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("qstat -xml -j %s: exit %d: %s", jobID, exit, strings.TrimSpace(string(stderr)))
	}
	return ParseGrantedGPUsXML(stdout, complexName)
}

// qstat -xml -j structure, minimal to the granted-resource subtree. Unmatched
// elements are ignored by encoding/xml, so this stays small on purpose.
type detailedJobXML struct {
	Jobs []struct {
		Tasks []struct {
			Granted []gruXML `xml:"JAT_granted_resources_list>element"`
		} `xml:"JB_ja_tasks>element"`
	} `xml:"djob_info>element"`
}

type gruXML struct {
	Name string `xml:"GRU_name"`
	Host string `xml:"GRU_host"`
	Map  []struct {
		Value string `xml:"RESL_value"`
	} `xml:"GRU_resource_map_list>element"`
}

// ParseGrantedGPUsXML extracts the granted GPUs for the named complex from
// `qstat -xml -j` output. Hosts appear in the order the scheduler granted them.
func ParseGrantedGPUsXML(data []byte, complexName string) ([]HostGPUs, error) {
	var doc detailedJobXML
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse qstat -xml -j: %w", err)
	}
	var out []HostGPUs
	for _, job := range doc.Jobs {
		for _, task := range job.Tasks {
			for _, gru := range task.Granted {
				if gru.Name != complexName {
					continue
				}
				devices := make([]int, 0, len(gru.Map))
				for ordinal, m := range gru.Map {
					// One RESL element is one granted device id (RESL_amount is 1
					// for RSMAP GPU ids).
					devices = append(devices, deviceIndex(m.Value, ordinal))
				}
				out = append(out, HostGPUs{Host: gru.Host, Devices: devices})
			}
		}
	}
	return out, nil
}

// resourceMapLine matches the flattened plain form emitted by `qstat -j`:
//
//	resource_map  1:  gpu=ocs-worker1=(0 1)
//
// captured as complex, host, and the space-separated id list.
var resourceMapLine = regexp.MustCompile(`^\s*resource_map\s+\d+:\s*([^=]+)=([^=]+)=\(([^)]*)\)`)

// ParseResourceMapPlain extracts granted GPUs from the plain `qstat -j` text for
// the named complex. It is the fallback when the XML view is unavailable; the
// XML form (ParseGrantedGPUsXML) is preferred because it is unambiguous.
func ParseResourceMapPlain(text, complexName string) []HostGPUs {
	var out []HostGPUs
	for _, line := range strings.Split(text, "\n") {
		m := resourceMapLine.FindStringSubmatch(line)
		if m == nil || strings.TrimSpace(m[1]) != complexName {
			continue
		}
		out = append(out, HostGPUs{
			Host:    strings.TrimSpace(m[2]),
			Devices: parseIDList(m[3]),
		})
	}
	return out
}

// GrantedGPUsPlain is the plain-text fallback for GrantedGPUs, reading
// `qstat -j <jobID>` and parsing its resource_map line. Used when the XML view
// fails to keep discovery working (REQ-FAB-003).
func GrantedGPUsPlain(ctx context.Context, r Runner, jobID, complexName string) ([]HostGPUs, error) {
	stdout, stderr, exit, err := r.Run(ctx, "qstat", "-j", jobID)
	if err != nil {
		return nil, fmt.Errorf("qstat -j %s: %w", jobID, err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("qstat -j %s: exit %d: %s", jobID, exit, strings.TrimSpace(string(stderr)))
	}
	return ParseResourceMapPlain(string(stdout), complexName), nil
}

// ParseSGEHGR parses the value of SGE_HGR_<complex> (e.g. "0 1") into device
// indices. This is the local exec host's view only; do not use it to build a
// multi-host layout (SI-19).
func ParseSGEHGR(value string) []int {
	return parseIDList(value)
}

// parseIDList turns a space-separated id list ("0 1 gpu2") into physical
// indices, applying the same numeric/ordinal rule as the XML path.
func parseIDList(s string) []int {
	fields := strings.Fields(s)
	ids := make([]int, 0, len(fields))
	for ordinal, f := range fields {
		ids = append(ids, deviceIndex(f, ordinal))
	}
	return ids
}

var trailingDigits = regexp.MustCompile(`(\d+)$`)

// deviceIndex maps an RSMAP id token to a physical device index. RSMAP ids are
// usually the numeric device index ("0", "1"); some sites name them ("gpu0"),
// in which case the trailing number is used. Anything else falls back to the
// token's ordinal position in the host's granted list so callers always get a
// usable contiguous-partitionable index.
func deviceIndex(token string, ordinal int) int {
	token = strings.TrimSpace(token)
	if n, err := strconv.Atoi(token); err == nil {
		return n
	}
	if m := trailingDigits.FindString(token); m != "" {
		if n, err := strconv.Atoi(m); err == nil {
			return n
		}
	}
	return ordinal
}
