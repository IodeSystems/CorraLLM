package config

import (
	"fmt"
	"strconv"
	"strings"
)

// sizeUnits maps a suffix to its byte multiplier. Decimal (GB) and binary (GiB)
// are both supported; a bare number is bytes. Internal consistency is what the
// residency ledger needs — declare pools and ramUsage in the same convention.
var sizeUnits = []struct {
	suffix string
	mult   int64
}{
	{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
	{"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3},
	{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10},
	{"B", 1},
}

// ParseSize parses a human size ("32GB", "16GiB", "512MB", "1024") into bytes.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	for _, u := range sizeUnits {
		if strings.HasSuffix(s, u.suffix) {
			num := strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, fmt.Errorf("size %q: %w", s, err)
			}
			return int64(f * float64(u.mult)), nil
		}
	}
	// No suffix → raw bytes.
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	return n, nil
}

// ParseSizes parses a pool→size map (pools, reserve, ramUsage) into bytes.
func ParseSizes(m map[string]string) (map[string]int64, error) {
	out := make(map[string]int64, len(m))
	for pool, s := range m {
		n, err := ParseSize(s)
		if err != nil {
			return nil, fmt.Errorf("pool %q: %w", pool, err)
		}
		out[pool] = n
	}
	return out, nil
}
