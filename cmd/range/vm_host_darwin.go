//go:build darwin && cgo

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/andreygrehov/range/internal/session"
	"github.com/andreygrehov/range/internal/vm"
)

// commandVMHost runs the VM of one session. range starts it from a copy of
// itself signed for Virtualization.framework, with the terminal inherited,
// and hands it the session on fd 3; readiness goes back on fd 4, and fd 5
// says when the banner is printed. It exits with the workload's status.
func commandVMHost(_ []string) error {
	configFile, ready, proceed := os.NewFile(3, "config"), os.NewFile(4, "ready"), os.NewFile(5, "go")
	if configFile == nil || ready == nil || proceed == nil {
		return errors.New(session.VMHostCommand + " is started by range, not by hand")
	}
	var cfg vm.HostConfig
	if err := json.NewDecoder(configFile).Decode(&cfg); err != nil {
		return fmt.Errorf("read the vm configuration: %w", err)
	}
	configFile.Close()

	machine, err := vm.StartVZ(cfg.Spec())
	if err != nil {
		return err
	}
	defer machine.Close()
	defer machine.Stop()

	result := make(chan error, 1)
	go func() {
		code, err := vm.Serve(machine, cfg, func() {
			ready.Write([]byte{'R'})
			io.ReadFull(proceed, make([]byte, 1))
		})
		if err == nil && code != 0 {
			err = session.ExitStatus(code)
		}
		result <- err
	}()
	select {
	case err = <-result:
	case <-parentGone():
		return errors.New("range ended; stopping its vm")
	}
	select {
	case <-machine.Stopped():
	case <-time.After(2 * time.Second):
	}
	return err
}

// parentGone is closed if the range that started this process ends without
// stopping it, killed outright, so its VM does not run on with no one to
// stop it.
func parentGone() <-chan struct{} {
	gone := make(chan struct{})
	parent := os.Getppid()
	go func() {
		for os.Getppid() == parent {
			time.Sleep(time.Second)
		}
		close(gone)
	}()
	return gone
}
