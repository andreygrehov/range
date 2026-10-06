package vm

import "testing"

func TestPortBase(t *testing.T) {
	for cmdline, want := range map[string]uint32{
		"console=hvc0 quiet rdinit=/init":                             DefaultPortBase,
		"console=hvc0 range.port=70000 quiet":                         70000,
		"range.port=0":                                                DefaultPortBase,
		"range.port=x":                                                DefaultPortBase,
		"range.portx=5 range.port=4096\n":                             4096,
		"console=hvc0 quiet loglevel=3 rdinit=/init range.port=65540": 65540,
	} {
		if got := PortBase(cmdline); got != want {
			t.Errorf("PortBase(%q) = %d, want %d", cmdline, got, want)
		}
	}
	if p := PortsFrom(100); p != (Ports{100, 101, 102, 103}) {
		t.Errorf("PortsFrom(100) = %+v", p)
	}
}
