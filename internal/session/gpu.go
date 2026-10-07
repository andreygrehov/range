package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/andreygrehov/range/internal/tool"
)

// An environment gets the host's NVIDIA GPUs the way nvidia-container-toolkit
// gives them to a container: the device nodes, and the driver's own libraries
// and tools, which must match the host's kernel module and so never come from
// the image. They appear at nvidiaDir, where CUDA images already look, and
// range adds it to LD_LIBRARY_PATH and PATH for every other image. All of it
// is mounted in the session's own mount namespace, which ends with the
// session and so takes it all away.

// nvidiaDir holds the driver's libraries in lib64, its tools in bin, and
// CUDA's compiled kernels in cache.
const nvidiaDir = "/usr/local/nvidia"

// CUDA compiles a program's kernels for this GPU when the program ships none
// built for it, and keeps them in CUDA_CACHE_PATH. In a container that cache
// goes with the container, so every run compiles again: llama.cpp's CUDA image
// on a T4 spends 110 s in it each time. Range keeps the cache on the host, so
// only the first run pays. 4 GiB is the most CUDA keeps.
const cudaCacheSize = "4294967296"

// driverLibraries are the libraries a CUDA program, NVML and nvidia-smi load:
// the compute and utility libraries of nvidia-container-toolkit.
var driverLibraries = []string{
	"libcuda.so", "libcudadebugger.so", "libnvidia-ml.so", "libnvidia-cfg.so",
	"libnvidia-nscq.so", "libnvidia-opencl.so", "libnvidia-gpucomp.so",
	"libnvidia-ptxjitcompiler.so", "libnvidia-fatbinaryloader.so", "libnvidia-allocator.so",
	"libnvidia-compiler.so", "libnvidia-nvvm.so", "libnvidia-pkcs11.so",
	"libnvidia-pkcs11-openssl3.so",
}

// driverTools are the driver's programs that work in an environment.
var driverTools = []string{
	"nvidia-smi", "nvidia-debugdump", "nvidia-persistenced",
	"nvidia-cuda-mps-control", "nvidia-cuda-mps-server",
}

// NVIDIA is the part of the host's NVIDIA driver an environment needs.
type NVIDIA struct {
	// Devices are device nodes, such as /dev/nvidia0.
	Devices []string `json:"devices"`
	// Files maps a file name in lib64 to the host file it shows.
	Files map[string]string `json:"files"`
	// Links maps a library's other names in lib64 to the file name it is.
	Links map[string]string `json:"links"`
	// Tools are host programs shown in bin.
	Tools []string `json:"tools"`
	// GPUs counts the GPU devices, and Version is the driver's.
	GPUs    int    `json:"gpus"`
	Version string `json:"version"`
	// Cache is the host directory that keeps CUDA's compiled kernels.
	Cache string `json:"cache"`
}

// ParseGPUs accepts what --gpus takes. Only "all" for now: one environment
// gets every GPU, and CUDA_VISIBLE_DEVICES picks among them.
func ParseGPUs(v string) error {
	if v != "all" {
		return errors.New(`only "all" is supported; pick GPUs with -e CUDA_VISIBLE_DEVICES=0,1`)
	}
	return nil
}

// FindNVIDIA finds this host's NVIDIA driver. Like nvidia-container-toolkit,
// it creates the unified memory device CUDA needs with nvidia-modprobe when
// nothing has used CUDA since the host started. CUDA's compiled kernels stay
// in cacheDir.
func FindNVIDIA(cacheDir string) (*NVIDIA, error) {
	if _, err := os.Stat("/dev/nvidia-uvm"); err != nil {
		if modprobe, err := exec.LookPath("nvidia-modprobe"); err == nil {
			_ = exec.Command(modprobe, "-u", "-c=0").Run()
		}
	}
	ldconfig, err := exec.LookPath("ldconfig")
	if err != nil {
		ldconfig = "/sbin/ldconfig"
	}
	cache, err := exec.Command(ldconfig, "-p").Output()
	if err != nil {
		return nil, fmt.Errorf("--gpus: list this host's libraries with ldconfig -p: %w", err)
	}
	d, err := findNVIDIA("/dev", parseLDCache(string(cache), runtime.GOARCH), exec.LookPath)
	if err != nil {
		return nil, err
	}
	d.Cache = filepath.Join(cacheDir, "cuda")
	return d, nil
}

// findNVIDIA finds the driver's devices in dev and its libraries among
// libraries, which maps names to paths as the host's loader cache does.
func findNVIDIA(dev string, libraries map[string]string, lookPath func(string) (string, error)) (*NVIDIA, error) {
	d := &NVIDIA{Files: map[string]string{}, Links: map[string]string{}}
	gpus, _ := filepath.Glob(filepath.Join(dev, "nvidia[0-9]*"))
	gpuNode := regexp.MustCompile(`^nvidia[0-9]+$`)
	for _, path := range gpus {
		if gpuNode.MatchString(filepath.Base(path)) {
			d.Devices = append(d.Devices, path)
			d.GPUs++
		}
	}
	if _, err := os.Stat(filepath.Join(dev, "nvidiactl")); err != nil || d.GPUs == 0 {
		return nil, errors.New("--gpus: this machine has no NVIDIA driver loaded: " +
			"/dev/nvidiactl and /dev/nvidia0 are missing")
	}
	if _, err := os.Stat(filepath.Join(dev, "nvidia-uvm")); err != nil {
		return nil, errors.New("--gpus: /dev/nvidia-uvm is missing, so CUDA cannot run; " +
			"load it with sudo nvidia-modprobe -u -c=0")
	}
	for _, name := range []string{"nvidiactl", "nvidia-uvm", "nvidia-uvm-tools", "nvidia-modeset"} {
		if path := filepath.Join(dev, name); exists(path) {
			d.Devices = append(d.Devices, path)
		}
	}
	sort.Strings(d.Devices)

	for name, path := range libraries {
		if !isDriverLibrary(name) {
			continue
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		file := filepath.Base(real)
		d.Files[file] = real
		if name != file {
			d.Links[name] = file
		}
	}
	cuda, ok := d.Links["libcuda.so.1"]
	if !ok {
		return nil, errors.New("--gpus: the NVIDIA driver's libcuda.so.1 is not in this host's loader cache")
	}
	if _, ok := d.Links["libcuda.so"]; !ok {
		d.Links["libcuda.so"] = cuda
	}
	d.Version = strings.TrimPrefix(cuda, "libcuda.so.")
	for _, name := range driverTools {
		if path, err := lookPath(name); err == nil {
			d.Tools = append(d.Tools, path)
		}
	}
	return d, nil
}

func isDriverLibrary(name string) bool {
	for _, lib := range driverLibraries {
		if name == lib || strings.HasPrefix(name, lib+".") {
			return true
		}
	}
	return false
}

// parseLDCache reads the output of ldconfig -p, keeping the libraries built
// for arch: name to path.
func parseLDCache(out, arch string) map[string]string {
	want := map[string]string{"amd64": "x86-64", "arm64": "AArch64"}[arch]
	libraries := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		// "\tlibcuda.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libcuda.so.1"
		left, path, ok := strings.Cut(strings.TrimSpace(line), " => ")
		if !ok {
			continue
		}
		name, tags, _ := strings.Cut(left, " ")
		if want != "" && !strings.Contains(tags, ","+want) {
			continue
		}
		if _, seen := libraries[name]; !seen {
			libraries[name] = path
		}
	}
	return libraries
}

// provideNVIDIA shows the driver inside the environment at root. It runs in
// the session's mount namespace, after /dev is in place.
func provideNVIDIA(root string, d *NVIDIA) error {
	for _, device := range d.Devices {
		target := filepath.Join(root, device)
		if exists(target) {
			continue // the host's whole /dev is already there
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, nil, 0o666); err != nil {
			return err
		}
		if err := tool.Run("mount", "--bind", device, target); err != nil {
			return fmt.Errorf("--gpus: show %s: %w", device, err)
		}
	}
	dir, err := makeDirIn(root, nvidiaDir)
	if err != nil {
		return err
	}
	if err := tool.Run("mount", "-t", "tmpfs", "-o", "mode=755,nosuid,nodev", "nvidia", dir); err != nil {
		return fmt.Errorf("--gpus: %w", err)
	}
	lib, bin, cache := filepath.Join(dir, "lib64"), filepath.Join(dir, "bin"), filepath.Join(dir, "cache")
	for _, sub := range []string{lib, bin, cache} {
		if err := os.Mkdir(sub, 0o755); err != nil {
			return err
		}
	}
	if d.Cache != "" {
		if err := os.MkdirAll(d.Cache, 0o755); err != nil {
			return err
		}
		if err := tool.Run("mount", "--bind", d.Cache, cache); err != nil {
			return fmt.Errorf("--gpus: keep CUDA's cache in %s: %w", d.Cache, err)
		}
	}
	show := func(source, target string) error {
		if err := os.WriteFile(target, nil, 0o755); err != nil {
			return err
		}
		// Read-only: the workload runs as root, and these are the host's.
		if err := tool.Run("mount", "--bind", "-o", "ro", source, target); err != nil {
			return fmt.Errorf("--gpus: show %s: %w", source, err)
		}
		return nil
	}
	for name, source := range d.Files {
		if err := show(source, filepath.Join(lib, name)); err != nil {
			return err
		}
	}
	for name, file := range d.Links {
		if err := os.Symlink(file, filepath.Join(lib, name)); err != nil {
			return err
		}
	}
	for _, source := range d.Tools {
		if err := show(source, filepath.Join(bin, filepath.Base(source))); err != nil {
			return err
		}
	}
	return nil
}

// withNVIDIAEnv adds nvidiaDir's lib64 and bin to the end of LD_LIBRARY_PATH
// and PATH, so the image's own entries come first, and points CUDA at its
// kept cache unless the image or the user says otherwise.
func withNVIDIAEnv(env []string) []string {
	add := map[string]string{"LD_LIBRARY_PATH": nvidiaDir + "/lib64", "PATH": nvidiaDir + "/bin"}
	defaults := map[string]string{"CUDA_CACHE_PATH": nvidiaDir + "/cache", "CUDA_CACHE_MAXSIZE": cudaCacheSize}
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		delete(defaults, key)
		if dir, ok := add[key]; ok {
			delete(add, key)
			switch value = strings.TrimSuffix(value, ":"); {
			case value == "":
				entry = key + "=" + dir
			case !strings.Contains(":"+value+":", ":"+dir+":"):
				entry = key + "=" + value + ":" + dir
			}
		}
		out = append(out, entry)
	}
	for _, key := range []string{"LD_LIBRARY_PATH", "PATH"} {
		if dir, ok := add[key]; ok {
			out = append(out, key+"="+dir)
		}
	}
	for _, key := range []string{"CUDA_CACHE_PATH", "CUDA_CACHE_MAXSIZE"} {
		if value, ok := defaults[key]; ok {
			out = append(out, key+"="+value)
		}
	}
	return out
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
