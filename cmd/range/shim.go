package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/andreygrehov/range/internal/session"
)

// shimCommand is how main hands a shim's call to commandShim.
const shimCommand = "__shim"

// commandShim is range inside a session, called by the name of a command
// the environment lacks: a shim. It asks the session for the image that
// holds the command, then runs it there.
func commandShim(name string, args []string) error {
	command, version, _ := strings.Cut(name, "@")
	conn, err := net.Dial("unix", session.InsideSocket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: command not found\n", name)
		return session.ExitStatus(127)
	}
	var reply session.InsideReply
	req := session.InsideRequest{Command: command, Version: version}
	if version == "" {
		cwd, _ := os.Getwd()
		req.Wants = session.Wanted(os.Getenv(session.UseVariable), cwd)
	}
	err = json.NewEncoder(conn).Encode(req)
	if err == nil {
		err = json.NewDecoder(conn).Decode(&reply)
	}
	conn.Close()
	if err == nil && reply.Error != "" {
		err = fmt.Errorf("%s", reply.Error)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "range: %s: %v\n", name, err)
		return session.ExitStatus(127)
	}
	if reply.First {
		why := ""
		if reply.From != "" {
			why = ", as " + reply.From + " asks"
		}
		fmt.Fprintf(os.Stderr, "\033[2m[range] %s from %s%s\033[0m\n", command, reply.Image, why)
	}
	return session.RunInside(reply, command, args)
}
