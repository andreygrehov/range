package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/andreygrehov/range/internal/session"
)

// range run -d starts the session in the background, as docker run -d does,
// and prints its ID once it is ready. The session is a range of its own,
// started again with the same arguments: it has no terminal, writes to its
// log, and says on fd 3 when it is ready.

// detachedEnv carries the ID of a detached session to the range that runs it.
const detachedEnv = "RANGE_DETACHED_ID"

var errDetach = errors.New("detach")

func startDetached(args []string, run bool) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	id := session.NewID()
	if err := os.MkdirAll(session.RegistryDir(), 0o700); err != nil {
		return err
	}
	logPath := session.LogPath(id)
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	verb := "shell"
	if run {
		verb = "run"
	}
	cmd := exec.Command(self, append([]string{verb}, args...)...)
	cmd.Env = append(os.Environ(), detachedEnv+"="+id)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.ExtraFiles = []*os.File{readyWrite}
	// Its own session: the terminal's hangup and Ctrl+C are not its.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	readyWrite.Close()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	ready := make(chan bool, 1)
	go func() {
		line, _ := bufio.NewReader(readyRead).ReadString('\n')
		ready <- strings.TrimSpace(line) == "ready"
	}()
	select {
	case ok := <-ready:
		if ok {
			fmt.Println(id)
			return nil
		}
		// The pipe closed without a word: the session ended before it was
		// ready. Its exit, and its log, say why.
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
	case <-exited:
	}
	data, _ := os.ReadFile(logPath)
	os.Stderr.Write(data)
	return errors.New("the session ended before it was ready")
}

// detachedReady tells the range that started this detached session that it
// is ready.
func detachedReady() {
	if os.Getenv(detachedEnv) == "" {
		return
	}
	if f := os.NewFile(3, "ready"); f != nil {
		f.Write([]byte("ready\n"))
		f.Close()
	}
}
