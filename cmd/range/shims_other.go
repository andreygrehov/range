//go:build !unix

package main

import "errors"

func refreshShims() {}

func runAsShim(string, []string) error { return errors.New("shims need a Unix system") }
