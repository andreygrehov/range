//go:build !linux

package session

import "errors"

func loopbackUp() error { return errors.New("a network namespace needs Linux") }

func waitForOwnNetwork(int) error { return errors.New("a network namespace needs Linux") }
