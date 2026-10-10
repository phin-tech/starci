package plan

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MinMemory is the least memory a step can be limited to: under it a
// container does not start.
const MinMemory = 6 << 20

// ParseTimeout reads how long a step may run, written as "90s", "10m",
// "1h30m". It returns whole seconds.
func ParseTimeout(s string) (seconds int, err error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a length of time; write it like 90s, 10m or 1h30m", s)
	}
	if d < time.Second || d%time.Second != 0 {
		return 0, fmt.Errorf("%q must be a whole number of seconds, and at least one", s)
	}
	return int(d / time.Second), nil
}

// ParseMemory reads an amount of memory, written as "512m" or "4g". The
// units are k, m and g, each 1024 of the one before.
func ParseMemory(s string) (bytes int64, err error) {
	bad := fmt.Errorf("%q is not an amount of memory; write it like 512m or 4g", s)
	if len(s) < 2 {
		return 0, bad
	}
	unit, ok := map[byte]int64{'k': 1 << 10, 'm': 1 << 20, 'g': 1 << 30}[strings.ToLower(s)[len(s)-1]]
	n, perr := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if !ok || perr != nil || n <= 0 || n > (1<<62)/unit {
		return 0, bad
	}
	if n*unit < MinMemory {
		return 0, fmt.Errorf("%q is less than %s, under which a container does not start", s, FormatMemory(MinMemory))
	}
	return n * unit, nil
}

// FormatMemory writes an amount of memory as ParseMemory reads it, in the
// largest unit that divides it.
func FormatMemory(bytes int64) string {
	for _, u := range []struct {
		size int64
		name string
	}{{1 << 30, "g"}, {1 << 20, "m"}, {1 << 10, "k"}} {
		if bytes >= u.size && bytes%u.size == 0 {
			return strconv.FormatInt(bytes/u.size, 10) + u.name
		}
	}
	return strconv.FormatInt(bytes, 10) + " bytes"
}

// FormatTimeout writes a number of seconds as ParseTimeout reads it, leaving
// out the parts that are zero: 5400 is "1h30m".
func FormatTimeout(seconds int) string {
	var out string
	for _, u := range []struct {
		size int
		name string
	}{{3600, "h"}, {60, "m"}, {1, "s"}} {
		if n := seconds / u.size; n > 0 {
			out += strconv.Itoa(n) + u.name
			seconds -= n * u.size
		}
	}
	if out == "" {
		return "0s"
	}
	return out
}
