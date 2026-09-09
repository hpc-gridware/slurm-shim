package gedata

import (
	"context"
	"encoding/xml"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// HostGPUs is the set of GPU device ids granted to a job on one exec host.
// Ids are the tokens the device-visibility variable consumes (REQ-GPU-002): the
// shim partitions them among the node's local ranks to build each rank's visible
// device set.
//
// They are strings, not ints, because both ends of the pipeline are strings. GE
// RSMAP ids are strings, and CUDA_VISIBLE_DEVICES / ROCR_VISIBLE_DEVICES accept
// either a numeric index or a "GPU-<hex>" UUID. Coercing to int in between
// destroyed the UUID form, which is the only device identity that survives a
// reboot, a driver reload, or an AMD compute-partition change.
type HostGPUs struct {
	Host    string
	Devices []string
	// Unrecognized holds granted ids that matched no known form and were given
	// their position in the grant instead. That keeps the job runnable, but the
	// id is a guess, so callers warn rather than passing it on silently: an id
	// the shim did not understand is how a device identity turns into the wrong
	// device.
	Unrecognized []string
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
				// One RESL element is one granted device id (RESL_amount is 1
				// for RSMAP GPU ids).
				raw := make([]string, 0, len(gru.Map))
				for _, m := range gru.Map {
					raw = append(raw, m.Value)
				}
				devices, unknown := DeviceTokens(raw)
				out = append(out, HostGPUs{
					Host: gru.Host, Devices: devices, Unrecognized: unknown,
				})
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
		devices, unknown := parseIDList(m[3])
		out = append(out, HostGPUs{
			Host:         strings.TrimSpace(m[2]),
			Devices:      devices,
			Unrecognized: unknown,
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
// ids. This is the local exec host's view only; do not use it to build a
// multi-host layout (SI-19).
func ParseSGEHGR(value string) (ids, unrecognized []string) {
	return parseIDList(value)
}

// parseIDList turns a space-separated id list ("0 1 gpu2") into device tokens,
// applying the same rule as the XML path.
func parseIDList(s string) (ids, unrecognized []string) {
	return DeviceTokens(strings.Fields(s))
}

// namedDevice matches the long-standing "named id" form: an alphabetic prefix,
// an optional hyphen or underscore, then digits ("gpu0", "gpu-1"). It is much
// narrower than the old rule, which coerced ANY token ending in digits and so
// silently mapped an id like "0000:c1:00.0" or "MIG-GPU-<uuid>/13/0" onto a
// device number having nothing to do with it.
//
// It cannot separate a deliberate name from a coincidence: "renderD128" has the
// same shape as "gpu128" and yields 128. That matches the behaviour these ids
// have always had, so the rule stays rather than breaking working sites, and the
// case is documented instead.
var namedDevice = regexp.MustCompile(`^[A-Za-z_]+[-_]?([0-9]+)$`)

// deviceUUID matches the device-UUID form both CUDA_VISIBLE_DEVICES and
// ROCR_VISIBLE_DEVICES accept. ROCr wants "GPU-" plus 16 hex digits and rejects a
// token longer than 20 characters; NVIDIA's form is longer and dashed. Require at
// least 8 hex digits so a hyphenated NAME such as "gpu-0" or "gpu-1" is not
// mistaken for a UUID -- those must keep falling through to the named-id rule, or
// an existing site's working ids would start reaching the runtime verbatim and
// match no device at all.
var deviceUUID = regexp.MustCompile(`^(?i:GPU)-[0-9a-fA-F]{8,}[0-9a-fA-F-]*$`)

// DeviceToken normalises one RSMAP id into the token the device-visibility
// variable will carry, and reports whether the id was understood.
//
// A numeric id is canonicalised ("007" -> "7"): the old int-based parser did this
// implicitly, and ROCr rejects a token that is not the canonical rendering of its
// index, discarding every id to its right, so passing "007" through would hand
// the job zero devices. A UUID is kept verbatim, because it is the only device
// identity stable across a reboot, a driver reload, or a partition-mode change.
// A named id such as "gpu0" keeps its historical coercion to the trailing digits.
//
// Anything else falls back to the token's ordinal position in the host's granted
// list, which keeps the grant usable but is a guess, and ok is false so callers
// warn rather than passing a guess on silently.
func DeviceToken(token string, ordinal int) (id string, ok bool) {
	token = strings.TrimSpace(token)
	if n, err := strconv.Atoi(token); err == nil {
		return strconv.Itoa(n), true
	}
	if deviceUUID.MatchString(token) {
		return token, true
	}
	if m := namedDevice.FindStringSubmatch(token); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return strconv.Itoa(n), true
		}
	}
	return strconv.Itoa(ordinal), false
}

// DeviceTokens normalises one host's granted ids, returning the tokens to publish
// and the raw ids that had to be guessed at.
func DeviceTokens(raw []string) (ids, unrecognized []string) {
	ids = make([]string, 0, len(raw))
	for ordinal, r := range raw {
		id, ok := DeviceToken(r, ordinal)
		ids = append(ids, id)
		if !ok {
			unrecognized = append(unrecognized, r)
		}
	}
	return ids, unrecognized
}
