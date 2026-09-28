package bytesize

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var units = []struct {
	suffix string
	factor int64
}{
	{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"TIB", 1 << 40},
	{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000}, {"TB", 1000 * 1000 * 1000 * 1000},
	{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40},
	{"B", 1},
}

// Parse reads a size such as 4096, 64KiB or 20GiB.
func Parse(value string) (int64, error) {
	text := strings.ToUpper(strings.TrimSpace(value))
	if text == "" {
		return 0, errors.New("empty size")
	}
	for _, unit := range units {
		if !strings.HasSuffix(text, unit.suffix) {
			continue
		}
		number := strings.TrimSpace(strings.TrimSuffix(text, unit.suffix))
		n, err := strconv.ParseFloat(number, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid size %q", value)
		}
		if n < 0 {
			return 0, fmt.Errorf("negative size %q", value)
		}
		return int64(n * float64(unit.factor)), nil
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", value)
	}
	return n, nil
}

// Format prints a size in decimal units, as 10.74 GB.
func Format(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for value := n / unit; value >= unit; value /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
