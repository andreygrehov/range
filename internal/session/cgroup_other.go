//go:build !linux

package session

import "errors"

type cgroup struct{}

func newCgroup(string, Limits) (*cgroup, error) {
	return nil, errors.New("a cgroup needs Linux")
}

func (*cgroup) fd() int       { return -1 }
func (*cgroup) remove() error { return nil }
