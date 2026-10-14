package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/andreygrehov/range/internal/commands"
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
	quiet = true
	run := []string{"--mount", cwd + ":" + cwd, "--workdir", cwd, "--workload", "x-" + name, image, "--", program}
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

// approved asks once, at a terminal, before a command runs from an image
// that is not an official one, and remembers the answer. Away from a
// terminal, nothing is asked and the image runs.
func approved(image string) bool {
	repo := commands.Repository(image)
	dir, err := commandsDir()
	if err != nil {
		return true
	}
	path := filepath.Join(dir, "approved")
	if data, err := os.ReadFile(path); err == nil && slices.Contains(strings.Fields(string(data)), repo) {
		return true
	}
	if !isTerminal(os.Stdin) || !isTerminal(os.Stderr) {
		return true
	}
	fmt.Fprintf(os.Stderr, "range: run %s, which is not an official image? [Y/n] ", repo)
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "" && a != "y" && a != "yes" {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintln(f, repo)
			f.Close()
		}
	}
	return true
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// commandInit prints the hook that sends a missing command to range x.
func commandInit(args []string) error {
	shell := "zsh"
	if len(args) > 0 {
		shell = args[0]
	}
	switch shell {
	case "zsh":
		fmt.Print("command_not_found_handler() { range x \"$@\"; }\n")
	case "bash":
		fmt.Print("command_not_found_handle() { range x \"$@\"; }\n")
	default:
		return fmt.Errorf("range init: no hook for %s yet; zsh or bash", shell)
	}
	return nil
}
