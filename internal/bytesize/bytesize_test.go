package bytesize

import (
	"testing"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		input string
		want  int64
		fails bool
	}{
		{"1024", 1024, false},
		{"1KiB", 1 << 10, false},
		{"64MiB", 64 << 20, false},
		{"10GiB", 10 << 30, false},
		{"1MB", 1000 * 1000, false},
		{"1.5MiB", 1572864, false},
		{"8m", 8 << 20, false},
		{" 4KiB ", 4 << 10, false},
		{"512B", 512, false},
		{"", 0, true},
		{"-5", 0, true},
		{"banana", 0, true},
		{"12XB", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got, err := Parse(tc.input)
			if tc.fails {
				if err == nil {
					t.Fatalf("parseSize(%q) = %d, want error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSize(%q): %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("parseSize(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1000, "1.00 kB"},
		{107374182400, "107.37 GB"},
		{18_420_000, "18.42 MB"},
	}
	for _, tc := range tests {
		if got := Format(tc.input); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
