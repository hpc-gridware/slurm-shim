// Package encoders implements the pure nodelist and tasks-per-node codecs the
// shim uses to speak SLURM's range syntax (spec section 8). It has no
// dependencies beyond the standard library so it can be exercised in isolation.
package encoders

import (
	"sort"
	"strconv"
	"strings"
)

// SortHosts orders short hostnames in natural (numeric-aware) order in place, so
// callers can feed an arbitrary set to CompressNodelist and still get contiguous
// ranges: [node10, node2, node1] -> [node1, node2, node10] -> "node[1-2,10]".
// Names sharing a (prefix, suffix) and both bearing a digit run are ordered by
// numeric value then digit width; anything else falls back to a lexical compare.
func SortHosts(hosts []string) {
	sort.SliceStable(hosts, func(i, j int) bool {
		pi, di, si, oki := splitName(hosts[i])
		pj, dj, sj, okj := splitName(hosts[j])
		if !oki || !okj || pi != pj || si != sj {
			return hosts[i] < hosts[j]
		}
		if vi, vj := mustAtoi(di), mustAtoi(dj); vi != vj {
			return vi < vj
		}
		return len(di) < len(dj)
	})
}

// CompressNodelist encodes an ordered list of short hostnames into SLURM range
// syntax, e.g. ["node001","node002","node003","node007"] -> "node[001-003,007]"
// (Encoder N1, REQ-ENC-001).
//
// Input order is preserved (REQ-ENC-002): names are grouped only while
// consecutive in the input and sharing the same (prefix, suffix); the shim does
// not sort. Within a group, numerically consecutive values collapse to a-b
// ranges, but a range additionally requires equal rendered width, so a width
// change breaks the range without breaking the surrounding bracket group
// (e.g. ["n8","n9","n10"] -> "n[8-9,10]"). Zero padding is preserved.
func CompressNodelist(hosts []string) string {
	var out []string
	for i := 0; i < len(hosts); {
		prefix, digits, suffix, ok := splitName(hosts[i])
		if !ok {
			// A name with no digit run cannot range; emit it verbatim.
			out = append(out, hosts[i])
			i++
			continue
		}

		// Gather the maximal run of consecutive names sharing (prefix, suffix).
		toks := []token{{digits: digits, val: mustAtoi(digits)}}
		j := i + 1
		for j < len(hosts) {
			p, d, s, k := splitName(hosts[j])
			if !k || p != prefix || s != suffix {
				break
			}
			toks = append(toks, token{digits: d, val: mustAtoi(d)})
			j++
		}

		if len(toks) == 1 {
			out = append(out, prefix+toks[0].digits+suffix)
		} else {
			out = append(out, prefix+"["+strings.Join(rangeItems(toks), ",")+"]"+suffix)
		}
		i = j
	}
	return strings.Join(out, ",")
}

type token struct {
	digits string
	val    int
}

// rangeItems collapses a group's tokens into range and single items, preserving
// input order. A range extends only across numerically consecutive values of
// equal rendered width (REQ-ENC-001).
func rangeItems(toks []token) []string {
	var items []string
	for k := 0; k < len(toks); {
		start := k
		for k+1 < len(toks) &&
			toks[k+1].val == toks[k].val+1 &&
			len(toks[k+1].digits) == len(toks[k].digits) {
			k++
		}
		if k == start {
			items = append(items, toks[start].digits)
		} else {
			items = append(items, toks[start].digits+"-"+toks[k].digits)
		}
		k++
	}
	return items
}

// splitName parses a hostname into prefix + last-maximal-digit-run + suffix.
// ok is false when the name contains no digits (e.g. "alpha"), in which case
// prefix holds the whole name.
func splitName(s string) (prefix, digits, suffix string, ok bool) {
	end := -1
	for i := len(s) - 1; i >= 0; i-- {
		if isDigit(s[i]) {
			end = i + 1
			break
		}
	}
	if end == -1 {
		return s, "", "", false
	}
	start := end
	for start > 0 && isDigit(s[start-1]) {
		start--
	}
	return s[:start], s[start:end], s[end:], true
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// mustAtoi parses an all-digit string. Overflow of very long runs is not a
// concern for real hostnames; callers pass digit substrings from splitName.
func mustAtoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
