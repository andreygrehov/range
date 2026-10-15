package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime"
)

// Behind a proxy that inspects TLS, as many companies run, a program inside
// an image trusts none of the sites it reaches: the proxy signs them with
// the company's own authority, which this machine trusts and the image does
// not. Range appends this machine's authorities to every bundle of them the
// environment has, in the session's own layer, so the image stays as it is.

// caBundles are where Linux distributions keep their bundle of trusted
// authorities: Debian, Ubuntu and Alpine; RHEL and Fedora; and others.
var caBundles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/cert.pem",
}

// HostCACerts returns the authorities this machine adds to an environment:
// those in RANGE_CA_FILE, or on Linux its own bundle. A Mac keeps its own in
// the keychain, so there only RANGE_CA_FILE adds any. RANGE_CA_FILE=none adds
// none.
func HostCACerts() ([]byte, error) {
	switch path := os.Getenv("RANGE_CA_FILE"); {
	case path == "none":
		return nil, nil
	case path != "":
		certs, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("RANGE_CA_FILE: %w", err)
		}
		return certs, nil
	case runtime.GOOS != "linux":
		return nil, nil
	}
	for _, path := range caBundles {
		if certs, err := os.ReadFile(path); err == nil {
			return certs, nil
		}
	}
	return nil, nil
}

// addCACerts appends certs to every bundle of authorities the environment at
// root has, once each, following links as the environment would. A bundle
// that has them already, as a named environment's does from its last
// session, stays as it is.
func addCACerts(root string, certs []byte) error {
	if len(bytes.TrimSpace(certs)) == 0 {
		return nil
	}
	done := map[string]bool{}
	for _, bundle := range caBundles {
		path, err := resolveIn(root, bundle)
		if errors.Is(err, os.ErrNotExist) || done[path] {
			continue
		}
		if err != nil {
			return err
		}
		done[path] = true
		have, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(have, bytes.TrimSpace(certs)) {
			continue
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_, err = f.Write(append(append([]byte("\n"), bytes.TrimSpace(certs)...), '\n'))
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("add this machine's authorities to %s: %w", bundle, err)
		}
	}
	return nil
}
