package vm

import "fmt"

// The host and the guest talk over vsock. The guest connects to the host on
// these ports: one carries control messages, the others the workload's
// standard streams. With a terminal, Stdin and Stdout carry the terminal and
// Stderr is unused.
const (
	PortControl = 1024
	PortStdin   = 1025
	PortStdout  = 1026
	PortStderr  = 1027
)

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
	Shares   []Share `json:"shares,omitempty"`
	Terminal bool    `json:"terminal,omitempty"`
	Rows     uint16  `json:"rows,omitempty"`
	Cols     uint16  `json:"cols,omitempty"`
	Term     string  `json:"term,omitempty"`
}

// Control messages after the config, one per line:
//
//	guest -> host  "ready"        the environment is up; the workload waits
//	host -> guest  "go"           the banner is printed; start the workload
//	host -> guest  "resize R C"   the terminal is now R rows by C columns
//	guest -> host  "exit N"       the workload exited with status N

// DiskName is the guest's name for disk i: vda, vdb, ...
func DiskName(i int) string { return "/dev/vd" + string(rune('a'+i)) }

// Share is a directory of the Mac shown read-write in the guest.
type Share struct {
	Tag    string `json:"tag"`
	Path   string `json:"path,omitempty"` // on the Mac
	Target string `json:"target"`         // in the environment
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
}
