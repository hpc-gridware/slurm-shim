package encoders

import (
	"fmt"
	"strings"
)

// ErrInvalidHostlist is returned for input that is not valid SLURM range
// syntax. Callers map it to the "scontrol: error: Invalid hostlist" message
// and exit 1 (spec section 8.2).
type ErrInvalidHostlist struct{ Reason string }

func (e ErrInvalidHostlist) Error() string { return "invalid hostlist: " + e.Reason }

// ExpandNodelist is the exact inverse of CompressNodelist over the same grammar:
// it returns one hostname per input list position, preserving order, so that
// ExpandNodelist(CompressNodelist(x)) == x for all valid x (Encoder N1',
// REQ-ENC-003). It supports a single bracket group per block (the only shape
// the compressor emits) plus verbatim non-bracketed names.
func ExpandNodelist(s string) ([]string, error) {
	blocks, err := splitTopLevel(s)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, b := range blocks {
		if b == "" {
			return nil, ErrInvalidHostlist{"empty block"}
		}
		open := strings.IndexByte(b, '[')
		if open < 0 {
			if strings.ContainsAny(b, "]") {
				return nil, ErrInvalidHostlist{"unmatched ']' in " + b}
			}
			out = append(out, b)
			continue
		}
		close := strings.IndexByte(b, ']')
		if close < 0 || close < open {
			return nil, ErrInvalidHostlist{"unmatched '[' in " + b}
		}
		prefix := b[:open]
		suffix := b[close+1:]
		inner := b[open+1 : close]
		if strings.ContainsAny(suffix, "[]") {
			return nil, ErrInvalidHostlist{"malformed brackets in " + b}
		}
		items := strings.Split(inner, ",")
		for _, it := range items {
			expanded, err := expandItem(prefix, it, suffix)
			if err != nil {
				return nil, err
			}
			out = append(out, expanded...)
		}
	}
	return out, nil
}

// expandItem expands a single bracket item, either "digits" or "start-end".
func expandItem(prefix, item, suffix string) ([]string, error) {
	dash := strings.IndexByte(item, '-')
	if dash < 0 {
		if !allDigits(item) {
			return nil, ErrInvalidHostlist{"non-numeric range value " + item}
		}
		return []string{prefix + item + suffix}, nil
	}
	lo, hi := item[:dash], item[dash+1:]
	if !allDigits(lo) || !allDigits(hi) {
		return nil, ErrInvalidHostlist{"non-numeric range " + item}
	}
	loN, hiN := mustAtoi(lo), mustAtoi(hi)
	if hiN < loN {
		return nil, ErrInvalidHostlist{"descending range " + item}
	}
	// Pad to the low endpoint's width; numbers wider than the pad print in full,
	// matching SLURM and preserving the compressor's zero padding on round-trip.
	width := len(lo)
	var out []string
	for n := loN; n <= hiN; n++ {
		out = append(out, fmt.Sprintf("%s%0*d%s", prefix, width, n, suffix))
	}
	return out, nil
}

// splitTopLevel splits on commas that are not inside a bracket group.
func splitTopLevel(s string) ([]string, error) {
	if s == "" {
		return nil, ErrInvalidHostlist{"empty input"}
	}
	var parts []string
	depth := 0
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth < 0 {
				return nil, ErrInvalidHostlist{"unmatched ']'"}
			}
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	if depth != 0 {
		return nil, ErrInvalidHostlist{"unmatched '['"}
	}
	parts = append(parts, s[start:])
	return parts, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}
