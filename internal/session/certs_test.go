package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddCACerts(t *testing.T) {
	corp := "-----BEGIN CERTIFICATE-----\ncorp\n-----END CERTIFICATE-----\n"
	debian := t.TempDir()
	os.MkdirAll(filepath.Join(debian, "etc/ssl/certs"), 0o755)
	os.WriteFile(filepath.Join(debian, "etc/ssl/certs/ca-certificates.crt"), []byte("public roots\n"), 0o644)
	// RHEL's bundle is a link into ca-trust.
	rhel := t.TempDir()
	os.MkdirAll(filepath.Join(rhel, "etc/pki/tls/certs"), 0o755)
	os.MkdirAll(filepath.Join(rhel, "etc/pki/ca-trust/extracted/pem"), 0o755)
	os.WriteFile(filepath.Join(rhel, "etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem"), []byte("public roots\n"), 0o644)
	os.Symlink("/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", filepath.Join(rhel, "etc/pki/tls/certs/ca-bundle.crt"))

	for name, tc := range map[string]struct{ root, bundle string }{
		"debian": {debian, "etc/ssl/certs/ca-certificates.crt"},
		"rhel":   {rhel, "etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem"},
	} {
		for i := 0; i < 2; i++ { // the second time, as a named environment's next session
			if err := addCACerts(tc.root, []byte(corp)); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		data, _ := os.ReadFile(filepath.Join(tc.root, tc.bundle))
		if !strings.HasPrefix(string(data), "public roots\n") || strings.Count(string(data), "corp") != 1 {
			t.Errorf("%s: bundle is %q", name, data)
		}
	}
	// An image with no bundle, such as one built from scratch, is left alone.
	if err := addCACerts(t.TempDir(), []byte(corp)); err != nil {
		t.Errorf("no bundle: %v", err)
	}
}

func TestHostCACerts(t *testing.T) {
	file := filepath.Join(t.TempDir(), "corp.pem")
	os.WriteFile(file, []byte("corp"), 0o644)
	t.Setenv("RANGE_CA_FILE", file)
	if certs, err := HostCACerts(); err != nil || string(certs) != "corp" {
		t.Errorf("RANGE_CA_FILE: %q, %v", certs, err)
	}
	t.Setenv("RANGE_CA_FILE", "none")
	if certs, err := HostCACerts(); err != nil || certs != nil {
		t.Errorf("none: %q, %v", certs, err)
	}
	t.Setenv("RANGE_CA_FILE", filepath.Join(t.TempDir(), "missing.pem"))
	if _, err := HostCACerts(); err == nil {
		t.Error("a missing RANGE_CA_FILE was not an error")
	}
}
