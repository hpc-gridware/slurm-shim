package gedata

import (
	"strconv"
	"strings"
)

// The value parsers below read Grid Engine's TIME and MEMORY types (sge_types)
// in one place, so every command reads "4g" and "2:00:00" the same way.
// go-clusterscheduler's helpers cover only part of these types (no T suffix,
// no plain-seconds or INFINITY times); once they do, these delegate.

// ParseTime reads a TIME value: seconds ("7200"), [[h:]m:]s ("2:00:00",
// "1::5") or INFINITY. ok is false for anything else, including "".
func ParseTime(v string) (seconds int64, unlimited, ok bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false, false
	}
	if strings.EqualFold(v, "INFINITY") {
		return 0, true, true
	}
	parts := strings.Split(v, ":")
	if len(parts) > 3 {
		return 0, false, false
	}
	for _, p := range parts {
		n := int64(0)
		if p != "" {
			var err error
			if n, err = strconv.ParseInt(p, 10, 64); err != nil || n < 0 {
				return 0, false, false
			}
		}
		seconds = seconds*60 + n
	}
	return seconds, false, true
}

// MemBytes reads a MEMORY value: a number with an optional K/M/G/T (powers
// of 1024) or k/m/g/t (powers of 1000) multiplier; a bare number is bytes.
func MemBytes(v string) (float64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	scale := 1.0
	switch v[len(v)-1] {
	case 'K':
		scale = 1 << 10
	case 'M':
		scale = 1 << 20
	case 'G':
		scale = 1 << 30
	case 'T':
		scale = 1 << 40
	case 'k':
		scale = 1e3
	case 'm':
		scale = 1e6
	case 'g':
		scale = 1e9
	case 't':
		scale = 1e12
	}
	if scale != 1 {
		v = v[:len(v)-1]
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n * scale, true
}
