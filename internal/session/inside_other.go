//go:build !linux

package session

import "errors"

// RunInside needs Linux: it runs inside a session.
func RunInside(InsideReply, string, []string) error {
	return errors.New("a command from another image runs inside a Linux session")
}
