package tlscert

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEphemeral(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"localhost", "api.fabric.microsoft.com", "onelake.dfs.fabric.microsoft.com"} {
		if err := leaf.VerifyHostname(h); err != nil {
			t.Errorf("cert does not cover %s: %v", h, err)
		}
	}
}

func TestLoadPersistsAndReuses(t *testing.T) {
	dir := t.TempDir()
	c1, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	l1, _ := x509.ParseCertificate(c1.Certificate[0])
	l2, _ := x509.ParseCertificate(c2.Certificate[0])
	if l1.SerialNumber.Cmp(l2.SerialNumber) != 0 {
		t.Fatal("second Load generated a new cert; want the persisted one (stable fingerprint)")
	}
}

func TestLoadFailureModes(t *testing.T) {
	// dataDir/tls exists as a FILE → MkdirAll fails.
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/tls", []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("Load with tls-path-is-a-file succeeded; want error")
	}

	// Corrupt persisted PEMs: LoadX509KeyPair fails, so Load regenerates
	// fresh ones over them rather than erroring.
	dir2 := t.TempDir()
	if err := os.MkdirAll(dir2+"/tls", 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(dir2+"/tls/cert.pem", []byte("garbage"), 0o644)
	os.WriteFile(dir2+"/tls/key.pem", []byte("garbage"), 0o600)
	if _, err := Load(dir2); err != nil {
		t.Fatalf("Load over corrupt PEMs = %v; want regeneration", err)
	}
}

// TestPersistedKeyIsNotWorldReadable.
//
// The private key is written 0600 and the certificate 0644, and nothing
// asserted it. That is a behavioural gap rather than a coverage one — the
// WriteFile calls are executed by every other test in this file, so the mode
// argument could be changed to 0644 without moving the coverage number at all
// while making the emulator's key readable by every account on the host.
func TestPersistedKeyIsNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); err != nil {
		t.Fatal(err)
	}

	ki, err := os.Stat(filepath.Join(dir, "tls", "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := ki.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("key.pem mode = %#o; the private key must not be group- or "+
			"world-readable", perm)
	}

	// The certificate is public by design, so this asserts only that it exists
	// and is readable — a key-like 0600 there would be a different bug.
	ci, err := os.Stat(filepath.Join(dir, "tls", "cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if ci.Mode().Perm()&0o400 == 0 {
		t.Errorf("cert.pem mode = %#o; want it readable", ci.Mode().Perm())
	}
}
