package session

import "testing"

func TestParsePort(t *testing.T) {
	for spec, want := range map[string]Port{
		"8000":              {"127.0.0.1", 8000, 8000},
		"8080:80":           {"127.0.0.1", 8080, 80},
		"0.0.0.0:8080:80":   {"0.0.0.0", 8080, 80},
		"::1:8080:80":       {}, // an IPv6 address needs brackets docker does not take either
		"localhost:8080:80": {},
		"80:0":              {},
		"70000":             {},
		"a:b":               {},
		"1:2:3:4":           {},
	} {
		got, err := ParsePort(spec)
		if want == (Port{}) {
			if err == nil {
				t.Errorf("ParsePort(%q) = %+v, want an error", spec, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("ParsePort(%q) = %+v, %v, want %+v", spec, got, err, want)
		}
	}
}

func TestParseEnv(t *testing.T) {
	t.Setenv("RANGE_TEST_FROM_HERE", "here")
	for spec, want := range map[string][3]string{
		"KEY=VALUE":            {"KEY", "VALUE", "set"},
		"EMPTY=":               {"EMPTY", "", "set"},
		"A=b=c":                {"A", "b=c", "set"},
		"RANGE_TEST_FROM_HERE": {"RANGE_TEST_FROM_HERE", "here", "set"},
		"RANGE_TEST_UNSET":     {"RANGE_TEST_UNSET", "", "unset"},
	} {
		key, value, ok, err := ParseEnv(spec)
		if err != nil || key != want[0] || value != want[1] || ok != (want[2] == "set") {
			t.Errorf("ParseEnv(%q) = %q, %q, %v, %v", spec, key, value, ok, err)
		}
	}
	for _, bad := range []string{"", "=x", "A B=c"} {
		if _, _, _, err := ParseEnv(bad); err == nil {
			t.Errorf("ParseEnv(%q) took it", bad)
		}
	}
}
