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

package slurmopts

import (
	"html"
	"regexp"
	"strings"
)

// An option heading in SchedMD's HTML man pages is a <dt> whose names are bold
// and which ends in an "OPT_" anchor:
//
//	<dt><B>-A</B>, <B>--account</B>=&lt;<I>account</I>&gt;<a class="slurm_link" id="OPT_account" ...
//	<dt><B>--exclusive</B>[={user|mcs|topo}]<a class="slurm_link" id="OPT_exclusive" ...
//	<dt><B>--json</B>, <B>--json</B>=<I>list</I>, ...<a class="slurm_link" id="OPT_json" ...
var (
	headingRE = regexp.MustCompile(`(?is)<dt>((?:[^<]|<[^d]|<d[^t])*?)<a class="slurm_link" id="OPT_`)
	boldRE    = regexp.MustCompile(`(?i)<B>([^<]*)</B>`)
	tagRE     = regexp.MustCompile(`<[^>]+>`)
)

// ParseManPage extracts the options of one SLURM HTML man page. Anchored
// headings that name no option (environment variables, format fields) are
// skipped. An option's kind comes from the text after its last spelling:
// "[=" is Optional, "=" or "<" is Required, nothing is None -- and a heading
// that lists the same spelling bare and with a value ("--json, --json=list")
// is Optional.
func ParseManPage(page string) []Option {
	var out []Option
	seen := map[string]bool{}
	for _, m := range headingRE.FindAllStringSubmatch(page, -1) {
		dt := m[1]
		var names []string
		bare, valued := map[string]bool{}, map[string]bool{}
		lastEnd := -1
		for _, b := range boldRE.FindAllStringSubmatchIndex(dt, -1) {
			text := html.UnescapeString(dt[b[2]:b[3]])
			if !strings.HasPrefix(text, "-") || len(text) < 2 {
				continue
			}
			// One bold run can hold several spellings ("-V, --version").
			parts := strings.Split(text, ",")
			for i, part := range parts {
				name, _, inlineVal := strings.Cut(strings.TrimSpace(part), "=")
				if !strings.HasPrefix(name, "-") || len(name) < 2 {
					continue
				}
				after := ""
				if i == len(parts)-1 {
					after = suffix(dt[b[1]:])
				}
				if inlineVal || strings.HasPrefix(after, "=") || strings.HasPrefix(after, "[=") {
					valued[name] = true
				} else {
					bare[name] = true
				}
				if !contains(names, name) {
					names = append(names, name)
				}
			}
			lastEnd = b[1]
		}
		if len(names) == 0 || seen[names[0]] {
			continue
		}
		kind := None
		after := suffix(dt[lastEnd:])
		switch {
		case strings.HasPrefix(after, "["):
			kind = Optional
		case after != "" || anyValued(names, valued):
			kind = Required
		}
		for _, n := range names {
			if bare[n] && valued[n] {
				kind = Optional
			}
		}
		for _, n := range names {
			seen[n] = true
		}
		out = append(out, Option{Names: names, Value: kind})
	}
	return out
}

// suffix is the plain text from s up to the next bold option name, trimmed.
func suffix(s string) string {
	if i := strings.Index(s, "<B>-"); i >= 0 {
		s = s[:i]
	}
	t := strings.TrimSpace(html.UnescapeString(tagRE.ReplaceAllString(s, "")))
	return strings.TrimSpace(strings.TrimPrefix(t, ","))
}

func anyValued(names []string, valued map[string]bool) bool {
	for _, n := range names {
		if valued[n] {
			return true
		}
	}
	return false
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
