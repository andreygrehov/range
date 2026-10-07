package session

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/andreygrehov/range/internal/tool"
)

// ChildCommand is the hidden subcommand range re-executes itself with inside
// fresh namespaces.
const ChildCommand = "__child"

// defaultPath is PATH in an environment that names none.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// childEnv is the environment the shell starts with. Sorted so it is stable.
func childEnv(cfg sessionConfig) []string {
	// Defaults first; anything the image declares wins, since an image that
	// ships a toolchain usually ships the PATH that finds it.
	defaults := map[string]string{
		"PATH":     defaultPath,
		"HOME":     "/root",
		"HOSTNAME": cfg.Hostname,
	}
	env := []string{}
	for _, key := range []string{"PATH", "HOME", "HOSTNAME"} {
		if _, declared := cfg.Environment[key]; !declared {
			env = append(env, key+"="+defaults[key])
		}
	}
	if term := os.Getenv("TERM"); term != "" {
		env = append(env, "TERM="+term)
	} else {
		env = append(env, "TERM=xterm")
	}
	keys := make([]string, 0, len(cfg.Environment))
	for key := range cfg.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+cfg.Environment[key])
	}
	if cfg.NVIDIA != nil {
		env = withNVIDIAEnv(env)
	}
	return env
}

// RunChild is the hidden second half of "range shell". It is re-executed by
// unshare(1), so it is already inside fresh mount, PID, UTS and IPC namespaces.
func RunChild(args []string) error {
	if len(args) == 0 {
		return errors.New(ChildCommand + ": missing session directory")
	}
	data, err := os.ReadFile(filepath.Join(args[0], "session.json"))
	if err != nil {
		return fmt.Errorf("read session: %w", err)
	}
	var cfg sessionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse session: %w", err)
	}
	exitWithParent()

	// Keep everything we mount from propagating back to the host.
	if err := tool.Run("mount", "--make-rprivate", "/"); err != nil {
		return err
	}
	for _, dir := range []string{"proc", "sys", "dev"} {
		if err := os.MkdirAll(filepath.Join(cfg.Root, dir), 0o755); err != nil {
			return err
		}
	}
	// We are already in the new PID namespace, so this /proc shows only our
	// own processes.
	if err := tool.Run("mount", "-t", "proc", "proc", filepath.Join(cfg.Root, "proc")); err != nil {
		return err
	}
	if err := mountMinimalDev(cfg.Root); err != nil {
		// Never fail to start a shell over this. A kernel without devpts
		// newinstance, or a host that will not mount tmpfs here, falls back to
		// the old behaviour and says so.
		log.Printf("range: minimal /dev unavailable (%v), binding the host's /dev instead", err)
		if err := tool.Run("mount", "--rbind", "/dev", filepath.Join(cfg.Root, "dev")); err != nil {
			return err
		}
		_ = tool.Run("mount", "--make-rslave", filepath.Join(cfg.Root, "dev"))
	}
	// Best effort: a missing /sys or an unsettable hostname is not worth
	// refusing the shell over.
	_ = tool.Run("mount", "-t", "sysfs", "sysfs", filepath.Join(cfg.Root, "sys"))
	provideResolvConf(cfg.Root)
	if cfg.NVIDIA != nil {
		if err := provideNVIDIA(cfg.Root, cfg.NVIDIA); err != nil {
			return err
		}
	}
	if cfg.Hostname != "" {
		_ = tool.Run("hostname", cfg.Hostname)
	}

	if err := syscall.Chroot(cfg.Root); err != nil {
		return fmt.Errorf("chroot %s: %w", cfg.Root, err)
	}
	if err := syscall.Chdir("/"); err != nil {
		return err
	}
	if cfg.Workdir != "" && cfg.Workdir != "/" {
		if err := syscall.Chdir(cfg.Workdir); err != nil {
			log.Printf("range: %s not present in the environment, starting in /", cfg.Workdir)
		}
	}

	argv := []string{cfg.Shell}
	if len(cfg.Command) > 0 {
		argv = cfg.Command
	}
	env := childEnv(cfg)
	// syscall.Exec takes a path, never a command name, so "range run ... -- go
	// version" has to be resolved against the environment's own PATH here. An
	// interactive shell never hit this because /bin/bash is a path and bash
	// does its own lookup once it is running.
	program, err := lookPathInEnvironment(argv[0], env)
	if err != nil {
		return err
	}
	// An artifact built for another architecture fails here with a bare
	// "exec format error", which says nothing about the actual problem.
	if err := checkExecutableArch(program); err != nil {
		return err
	}
	signalReady()
	return syscall.Exec(program, argv, env)
}

// mountMinimalDev gives the environment the device nodes a program expects and
// nothing else.
//
// Rbinding the host's /dev is what a quick prototype does, and it hands the
// workload every disk, every loop device and every NBD device on the machine.
// This is still not a security boundary - there is no user namespace, and the
// shell runs as root - but a workload no longer has the host's block devices
// sitting in front of it by default.
func mountMinimalDev(root string) error {
	dev := filepath.Join(root, "dev")
	if err := tool.Run("mount", "-t", "tmpfs", "-o", "mode=755,nosuid", "dev", dev); err != nil {
		return err
	}
	for _, node := range []string{"null", "zero", "full", "random", "urandom", "tty"} {
		target := filepath.Join(dev, node)
		file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, 0o666)
		if err != nil {
			continue
		}
		file.Close()
		// A bind of the host node keeps the right major/minor without needing
		// mknod, which a container runtime would use here.
		_ = tool.Run("mount", "--bind", filepath.Join("/dev", node), target)
	}
	if err := os.MkdirAll(filepath.Join(dev, "pts"), 0o755); err != nil {
		return err
	}
	if err := tool.Run("mount", "-t", "devpts", "-o", "newinstance,ptmxmode=0666,mode=0620",
		"devpts", filepath.Join(dev, "pts")); err != nil {
		// Without a private devpts there is no usable terminal, and a shell
		// without a terminal is not worth handing back.
		return fmt.Errorf("devpts: %w", err)
	}
	_ = tool.Run("mount", "--bind", filepath.Join(dev, "pts", "ptmx"), filepath.Join(dev, "ptmx"))
	for _, link := range [][2]string{
		{"/proc/self/fd", "fd"}, {"/proc/self/fd/0", "stdin"},
		{"/proc/self/fd/1", "stdout"}, {"/proc/self/fd/2", "stderr"},
	} {
		_ = os.Symlink(link[0], filepath.Join(dev, link[1]))
	}
	if err := os.MkdirAll(filepath.Join(dev, "shm"), 0o1777); err != nil {
		return err
	}
	_ = tool.Run("mount", "-t", "tmpfs", "-o", "mode=1777,nosuid,nodev", "shm",
		filepath.Join(dev, "shm"))
	return nil
}

// lookPathInEnvironment resolves a bare command name against the PATH the
// environment declares, from inside it. exec.LookPath cannot be used: after the
// chroot this process still carries the host's PATH, which names directories
// that mean something entirely different in here.
func lookPathInEnvironment(name string, env []string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	search := ""
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			search = value
		}
	}
	if search == "" {
		search = defaultPath
	}
	for _, dir := range strings.Split(search, ":") {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found in this environment; PATH is %s", name, search)
}

// elfMachines maps the ELF e_machine values Range can run to their Go names.
var elfMachines = map[uint16]string{
	0x03: "386", 0x28: "arm", 0x3e: "amd64", 0xb7: "arm64", 0xf3: "riscv64",
}

// checkExecutableArch reads the ELF header of the program about to be executed
// and reports a mismatch in terms of the architectures involved. Without this
// the kernel refuses the exec with ENOEXEC and the user is told only "exec
// format error", which does not mention architecture at all.
func checkExecutableArch(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s is not present in this environment: %w", path, err)
	}
	name := executableArch(path)
	if name == "" || name == runtime.GOARCH {
		return nil
	}
	return fmt.Errorf("the programs here are built for linux/%s, and this machine runs linux/%s.\n"+
		"  Build an artifact for this machine with range build --platform linux/%s, on any host.\n"+
		"  A disk of another machine, such as an EBS snapshot, opens with Range's own tools in\n"+
		"  Range's VM: RANGE_RUNTIME=kvm on Linux, or on a Mac",
		name, runtime.GOARCH, runtime.GOARCH)
}

// executableArch names the architecture an ELF program is built for, or ""
// for a script, or anything else this cannot tell.
func executableArch(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	header := make([]byte, 20)
	if _, err := io.ReadFull(file, header); err != nil || string(header[:4]) != "\x7fELF" {
		return ""
	}
	machine := binary.LittleEndian.Uint16(header[18:20])
	if header[5] == 2 { // big-endian ELF
		machine = binary.BigEndian.Uint16(header[18:20])
	}
	return elfMachines[machine]
}

// provideResolvConf gives the environment the host's resolver. An artifact has
// to stay portable across networks, so it must not carry a baked-in
// /etc/resolv.conf; the runtime supplies one, the way containers do. On a
// typical distro image the path is a symlink into an empty /run, so it is
// replaced with a regular file in the writable layer before binding.
func provideResolvConf(root string) {
	const source = "/etc/resolv.conf"
	if _, err := os.Stat(source); err != nil {
		return
	}
	target := filepath.Join(root, "etc", "resolv.conf")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return
	}
	if info, err := os.Lstat(target); err != nil || info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
		os.RemoveAll(target)
		if err := os.WriteFile(target, nil, 0o644); err != nil {
			return
		}
	}
	if err := tool.Run("mount", "--bind", source, target); err != nil {
		log.Printf("range: could not provide DNS configuration: %v", err)
	}
}

// signalReady tells the parent the environment is usable and waits for it to
// finish printing, so the banner never lands on top of the first prompt.
func signalReady() {
	ready := os.NewFile(3, "range-ready")
	proceed := os.NewFile(4, "range-go")
	if ready == nil || proceed == nil {
		return // launched without the handshake pipes
	}
	if _, err := ready.Write([]byte{'R'}); err != nil {
		return
	}
	ready.Close()
	io.ReadFull(proceed, make([]byte, 1))
	proceed.Close()
}

// runInNamespaces re-executes this binary inside fresh namespaces. unshare(1)
// does the cloning so that this package stays free of Linux-only Go symbols
// and builds on every platform.
//
// Two inherited pipes carry a handshake: the child reports readiness on fd 3
// once it is about to exec the workload, and waits on fd 4 until the parent has
// printed its banner. Without the second half the banner races the shell prompt.
func runInNamespaces(ctx context.Context, sessionDir string, controllingTerminal bool, onReady func()) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	defer readyRead.Close()
	goRead, goWrite, err := os.Pipe()
	if err != nil {
		readyWrite.Close()
		return err
	}
	defer goWrite.Close()

	cmd := exec.CommandContext(ctx, "unshare",
		"--mount", "--uts", "--ipc", "--pid", "--fork",
		self, ChildCommand, sessionDir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{readyWrite, goRead}
	if controllingTerminal {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	}
	cmd.SysProcAttr = dieWithParent(cmd.SysProcAttr)
	// The death signal follows the thread that starts unshare, not the
	// process: keep this goroutine on it until unshare ends.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := cmd.Start(); err != nil {
		readyWrite.Close()
		goRead.Close()
		return err
	}
	readyWrite.Close()
	goRead.Close()

	go func() {
		// A closed pipe without a byte means the child died before readiness;
		// cmd.Wait reports the real error in that case.
		if _, err := io.ReadFull(readyRead, make([]byte, 1)); err != nil {
			return
		}
		onReady()
		goWrite.Write([]byte{'G'})
	}()
	return exitStatusOf(cmd.Wait())
}
