package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/andreygrehov/range/internal/commands"
	"github.com/andreygrehov/range/internal/oci"
	"github.com/andreygrehov/range/internal/session"
)

// "range x COMMAND[@VERSION] ARGS..." runs a command this machine does not
// have, from the container image that holds it, with the current directory
// at the same path inside. The catalog's command map says which image: the
// first one built for this platform. A version is a tag: python@2.7 runs in
// python:2.7. The image opens lazily, so only what the command reads
// crosses the network.
//
// "range init zsh" hooks it into a shell, so a missing command runs this
// way by its bare name.

// quiet leaves out the session banner and summary: the command's own output
// is all there is.
var quiet bool

func commandX(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: range x COMMAND[@VERSION] [ARGS...]")
	}
	name, version, _ := strings.Cut(args[0], "@")
	image, program, official, err := xResolve(context.Background(), name, version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "range: %s: %v\n", args[0], err)
		return session.ExitStatus(127) // as a shell does
	}
	if !official && !approved(image) {
		return session.ExitStatus(126)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\033[2m[range] %s from %s\033[0m\n", args[0], image)
	quiet, oci.Quiet = true, true
	run := []string{"--workdir", cwd, "--workload", "x-" + name}
	// Under sudo, the command runs as the one who ran sudo, so what it writes
	// is theirs, not root's, and sees their home.
	home, homeErr := os.UserHomeDir()
	if uid, gid := os.Getenv("SUDO_UID"), os.Getenv("SUDO_GID"); os.Geteuid() == 0 && uid != "" {
		run = append(run, "--user", uid+":"+gid)
		if u, err := user.LookupId(uid); err == nil {
			home, homeErr = u.HomeDir, nil
		}
	}
	// The home directory is inside, at its own path, for the files a command
	// reads there: ~/.gitconfig, ~/.kube, ~/.aws. What the command writes
	// there goes with the session, so a Linux program never fills a Mac's
	// home. The current directory then goes on top of it, for real.
	if homeErr == nil && home != "/" {
		run = append(run, "--mount", home+":"+home+":overlay", "-e", "HOME="+home)
	}
	run = append(run, "--mount", cwd+":"+cwd)
	// A proxy this shell uses, the command needs too.
	for _, key := range []string{"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy"} {
		if _, set := os.LookupEnv(key); set {
			run = append(run, "-e", key)
		}
	}
	run = append(run, image, "--", program)
	return commandRun(append(run, args[1:]...))
}

// xResolve finds the image and program for a command in the catalog's map.
// A command the map does not hold, with a version, is tried as an image of
// the same name: gcc@4.9 is gcc:4.9.
func xResolve(ctx context.Context, name, version string) (image, program string, official bool, err error) {
	client, err := commandsClient()
	if err != nil {
		return "", "", false, err
	}
	owners, err := client.Lookup(ctx, name)
	if errors.Is(err, commands.ErrUnknown) && version != "" {
		return name + ":" + version, name, !strings.Contains(name, "/"), nil
	}
	if errors.Is(err, commands.ErrUnknown) {
		return "", "", false, errors.New("command not found")
	}
	if err != nil {
		return "", "", false, err
	}
	return pickOwner(owners, "linux/"+runtime.GOARCH, name, version)
}

// pickOwner takes the first owner built for platform. Its tag is the version
// asked for, or the one the catalog read.
func pickOwner(owners []commands.Owner, platform, name, version string) (image, program string, official bool, err error) {
	for _, o := range owners {
		if !slices.Contains(o.Platforms, platform) {
			continue
		}
		tag := version
		if tag == "" {
			tag = o.Tag
		}
		if tag == "" {
			tag = "latest"
		}
		program = name
		if o.Path != "" && version == "" {
			program = o.Path
		}
		return o.Repo + ":" + tag, program, !strings.Contains(o.Repo, "/"), nil
	}
	return "", "", false, fmt.Errorf("no image of it is built for %s", platform)
}

// approved says whether a command may run from an image that is not an
// official one. Range asks once, at a terminal, and remembers the answer.
// Away from a terminal, as in a script or a Makefile, only an image allowed
// before runs: a typo in a build must not run a stranger's image unseen.
func approved(image string) bool {
	repo := commands.Repository(image)
	path, err := allowedPath()
	if err != nil {
		return false
	}
	if data, err := os.ReadFile(path); err == nil && slices.Contains(strings.Fields(string(data)), repo) {
		return true
	}
	if !isTerminal(os.Stdin) || !isTerminal(os.Stderr) {
		fmt.Fprintf(os.Stderr, "range: %s is not an official image: allow it once at a terminal, or with range commands allow %s\n", repo, repo)
		return false
	}
	fmt.Fprintf(os.Stderr, "range: run %s, which is not an official image? [Y/n] ", repo)
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "" && a != "y" && a != "yes" {
		return false
	}
	return allow(repo) == nil
}

// allowedPath is the list of repositories allowed to run commands.
func allowedPath() (string, error) {
	dir, err := commandsDir()
	return filepath.Join(dir, "allowed"), err
}

// allow adds a repository to the list.
func allow(repo string) error {
	path, err := allowedPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(f, repo)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// commandInit prints what a shell needs to run a missing command: a hook
// for what one types, and the shims at the end of PATH for what scripts
// run. A shell runs it at every start, so it never waits on the network: it
// refreshes the shims in the background once a day.
func commandInit(args []string) error {
	shell := "zsh"
	if len(args) > 0 {
		shell = args[0]
	}
	hook := map[string]string{"zsh": "command_not_found_handler", "bash": "command_not_found_handle"}[shell]
	if hook == "" {
		return fmt.Errorf("range init: no hook for %s yet; zsh or bash", shell)
	}
	dir, err := shimsDir()
	if err != nil {
		return err
	}
	fmt.Printf("%s() { range x \"$@\"; }\n", hook)
	fmt.Printf("case \":$PATH:\" in *:%q:*) ;; *) export PATH=\"$PATH:\"%q ;; esac\n", dir, dir)
	if info, err := os.Stat(dir); err != nil || time.Since(info.ModTime()) > 24*time.Hour {
		refreshShims()
	}
	return nil
}

// shimsDir holds a shim for every command in the map.
func shimsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "range", "shims"), nil
}

// commandShims makes a shim for every command in the catalog's map: a link
// to range, which runs a command when it is called by its name. The shims
// are swapped in whole, so PATH never sees half of them.
func commandShims(args []string) error {
	if len(args) > 0 {
		return errors.New("usage: range shims")
	}
	client, err := commandsClient()
	if err != nil {
		return err
	}
	names, err := client.Names(context.Background())
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	dir, err := shimsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	fresh, err := os.MkdirTemp(filepath.Dir(dir), ".shims-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(fresh)
	made := 0
	for _, name := range names {
		if strings.HasPrefix(name, "range") {
			continue // range itself, under its own name, is no shim
		}
		err := os.Symlink(self, filepath.Join(fresh, name))
		if errors.Is(err, os.ErrExist) {
			// A Mac's file system ignores case by default: python3 and
			// PYTHON3 are one name there. The first stays.
			continue
		}
		if err != nil {
			return err
		}
		made++
	}
	if err := os.Chmod(fresh, 0o755); err != nil {
		return err
	}
	old := fresh + ".old"
	if err := os.Rename(dir, old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(fresh, dir); err != nil {
		return err
	}
	os.RemoveAll(old)
	fmt.Printf("%d shims in %s\n", made, dir)
	return nil
}
