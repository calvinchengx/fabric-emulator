package tlscert

import (
	"crypto/x509"
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
