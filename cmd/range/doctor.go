package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/session"
)

func commandDoctor(args []string) error {
	c, err := core.ReadConfig()
	if err != nil {
		return err
	}
	host := session.Select()
	fmt.Printf("Host        %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("Runtime     %s\n", host.Name())
	switch host.Name() {
	case "native":
		fmt.Printf("Execution   native namespaces on this kernel\n")
	case "vz":
		fmt.Printf("Execution   a Linux VM per session (Virtualization.framework), artifact served over NBD\n")
	default:
		fmt.Printf("Execution   Linux VM, artifact served from this host over NBD\n")
	}
	fmt.Printf("Backend     host-side Range Core\n")
	fmt.Printf("Cache       %s\n", c.CacheDir)
	fmt.Printf("Profiles    %s\n", filepath.Join(c.CacheDir, "profiles"))
	fmt.Println()

	ready := true
	fmt.Println("Needs")
	for _, req := range host.Requirements() {
		mark := "ok"
		if !req.OK {
			mark = "MISSING"
			if req.Fixable {
				mark = "will provision"
			} else {
				ready = false
			}
		}
		fmt.Printf("  %-16s %-15s %s\n", req.What, mark, req.From)
		if !req.OK {
			for _, line := range strings.Split(req.Detail, "\n") {
				fmt.Printf("  %-16s %s\n", "", line)
			}
		}
	}
	fmt.Println()
	fmt.Println("Does not need")
	fmt.Printf("  %s\n\n", session.NotNeeded(host))
	if ready {
		fmt.Println("This host can run range shell.")
		return nil
	}
	return errors.New("this host is not ready; address the items above")
}
