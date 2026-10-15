package vm

import (
	"fmt"
	"strconv"
	"strings"
)

// The host and the guest talk over vsock. The guest connects to the host on
// four ports from a base: one carries control messages, the others the
// workload's standard streams. With a terminal, stdin and stdout carry the
// terminal and stderr is unused. On a Mac each VM has vsock ports of its own
// and the base is DefaultPortBase; on Linux the host's vsock ports are shared
// by every VM, so each session has a base of its own, on the kernel command
// line as range.port.
const (
	DefaultPortBase = 1024

	portControl = 0
	portStdin   = 1
	portStdout  = 2
	portStderr  = 3
	// portCommands serves the commands a session runs from other images,
	// and from portImages on, one port each, the images they come from.
	portCommands = 4
	// portEgress carries a session's way out when it has no network: the
	// guest relays its proxy's connections to the host's.
	portEgress = 5
	// portExec carries range exec and range cp: the host connects, the
	// guest runs the command in the session.
	portExec   = 6
	portImages = 8
	// portSpan is how many ports a session's base keeps: a base is a
	// multiple of it.
	portSpan = 64

	// PortRelay+i carries connections to the i-th published port: the host
	// connects, and the guest relays to that port on its own 127.0.0.1.
	PortRelay = 2000
)

// Ports are the vsock ports of one session.
type Ports struct{ Control, Stdin, Stdout, Stderr, Commands, Egress, Exec uint32 }

// PortsFrom returns the ports of a session from its base.
func PortsFrom(base uint32) Ports {
	return Ports{base + portControl, base + portStdin, base + portStdout, base + portStderr,
		base + portCommands, base + portEgress, base + portExec}
}

// PortBase reads range.port from a kernel command line, or DefaultPortBase.
func PortBase(cmdline string) uint32 {
	for _, field := range strings.Fields(cmdline) {
		if v, ok := strings.CutPrefix(field, "range.port="); ok {
			if n, err := strconv.ParseUint(v, 10, 32); err == nil && n > 0 {
				return uint32(n)
			}
		}
	}
	return DefaultPortBase
}

// GuestConfig is the first line on the control connection: what the guest
// runs. The environment is /dev/vda; mount i is the disk after it.
type GuestConfig struct {
	SessionID      string   `json:"session"`
	Workload       string   `json:"workload"`
	Workdir        string   `json:"workdir,omitempty"`
	ShellPath      string   `json:"shell,omitempty"`
	Command        []string `json:"command,omitempty"`
	DefaultCommand bool     `json:"default_command,omitempty"`
	Mounts         []string `json:"mounts,omitempty"`
	// Shares are directories of the Mac, by virtiofs tag, and where they
	// appear in the environment.
	Shares []Share `json:"shares,omitempty"`
	// Env sets variables, KEY=VALUE; Ports are published, by number inside.
	Env      []string `json:"env,omitempty"`
	Ports    []int    `json:"ports,omitempty"`
	Terminal bool     `json:"terminal,omitempty"`
	Rows     uint16   `json:"rows,omitempty"`
	Cols     uint16   `json:"cols,omitempty"`
	Term     string   `json:"term,omitempty"`
	// Commands says the host serves commands from other images.
	Commands bool `json:"commands,omitempty"`
	// CACerts are authorities the host trusts, for the environment's bundle.
	CACerts []byte `json:"ca_certs,omitempty"`
	// User is UID:GID to run the workload as, or "".
	User string `json:"user,omitempty"`
	// Disk is the most the writable layer holds, in bytes, or 0.
	Disk uint64 `json:"disk,omitempty"`
	// NoNetwork says the VM has no network device. Egress says the host
	// serves a proxy on the egress port, for the guest to offer at
	// 127.0.0.1:3128.
	NoNetwork bool `json:"no_network,omitempty"`
	Egress    bool `json:"egress,omitempty"`
	// Idle runs no workload: the session waits for range exec.
	Idle bool `json:"idle,omitempty"`
}

// Control messages after the config, one per line:
//
//	guest -> host  "ready"        the environment is up; the workload waits
//	host -> guest  "go"           the banner is printed; start the workload
//	host -> guest  "resize R C"   the terminal is now R rows by C columns
//	host -> guest  "signal N"     deliver signal N to the workload
//	host -> guest  "stop N"       end the session: SIGTERM, SIGKILL after N s
//	guest -> host  "exit N"       the workload exited with status N

// DiskName is the guest's name for disk i: vda, vdb, ...
func DiskName(i int) string { return "/dev/vd" + string(rune('a'+i)) }

// Spec is the VM to boot.
type Spec struct {
	Kernel, Initrd string
	CPUs           uint
	Memory         uint64
	// Disks are nbd:// URLs, attached read-only in order as vda, vdb, ...
	Disks []string
	// Console is a file for the kernel's and init's output; "" discards it.
	Console string
	// Shares are directories of the host the guest mounts with virtiofs.
	Shares []Share
	// NoNetwork leaves the VM without a network device.
	NoNetwork bool
}

// Share is a directory of the host shown read-write in the guest.
type Share struct {
	Tag     string `json:"tag"`
	Path    string `json:"path,omitempty"` // on the Mac
	Target  string `json:"target"`         // in the environment
	Overlay bool   `json:"overlay,omitempty"`
}

// ShareTag is the virtiofs tag of share i.
func ShareTag(i int) string { return fmt.Sprintf("range%d", i) }

// HostConfig is what range hands the process that runs the VM: the machine,
// and the session for the guest. The process reads it as JSON from fd 3,
// reports readiness on fd 4 and waits on fd 5 for the banner.
type HostConfig struct {
	Kernel  string      `json:"kernel"`
	Initrd  string      `json:"initrd"`
	CPUs    uint        `json:"cpus"`
	Memory  uint64      `json:"memory"`
	Disks   []string    `json:"disks"`
	Console string      `json:"console,omitempty"`
	Guest   GuestConfig `json:"guest"`
	// PortBase is the session's first vsock port; 0 means DefaultPortBase.
	PortBase uint32 `json:"port_base,omitempty"`
	// Publish is where on the Mac each of Guest.Ports is published.
	Publish []Publish `json:"publish,omitempty"`
	// ServeCommands asks for commands from other images. Commands serves
	// them: a process that reads this configuration as JSON makes its own.
	ServeCommands bool     `json:"serve_commands,omitempty"`
	Commands      Commands `json:"-"`
	// AllowHosts are the hosts the session may reach through the host's
	// proxy, as --allow-host gives them, when Guest.Egress is set.
	AllowHosts []string `json:"allow_hosts,omitempty"`
	// ControlSocket is where range exec, range cp and range stop reach the
	// session, or "": each request goes on to the guest.
	ControlSocket string `json:"control_socket,omitempty"`
	// EgressLog is told of each request through that proxy, or nil.
	EgressLog func(host string, port int, allowed bool) `json:"-"`
}

// Spec is the machine cfg describes.
func (cfg HostConfig) Spec() Spec {
	return Spec{Kernel: cfg.Kernel, Initrd: cfg.Initrd, CPUs: cfg.CPUs, Memory: cfg.Memory,
		Disks: cfg.Disks, Console: cfg.Console, Shares: cfg.Guest.Shares, NoNetwork: cfg.Guest.NoNetwork}
}

// Publish is a port of the guest published on the Mac at HostIP:HostPort.
type Publish struct {
	HostIP   string `json:"host_ip"`
	HostPort int    `json:"host_port"`
}
