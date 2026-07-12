// Package tlscert generates the emulator's self-signed TLS certificate
// (localhost + the Fabric host names), persisted under dataDir/tls when a
// data directory is configured so the fingerprint stays stable across runs.
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Hosts the certificate covers — local addressing plus the real Fabric host
// names so /etc/hosts-style redirection works.
var Hosts = []string{
	"localhost", "fabric-emulator",
	"api.fabric.microsoft.com", "onelake.dfs.fabric.microsoft.com", "onelake.blob.fabric.microsoft.com",
}

// Load returns a certificate, generating (and persisting when dataDir is
// non-empty) one if needed.
func Load(dataDir string) (tls.Certificate, error) {
	if dataDir != "" {
		certPath := filepath.Join(dataDir, "tls", "cert.pem")
		keyPath := filepath.Join(dataDir, "tls", "key.pem")
		if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
			return c, nil
		}
		certPEM, keyPEM, err := generate()
		if err != nil {
			return tls.Certificate{}, err
		}
		if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
			return tls.Certificate{}, err
		}
		if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
			return tls.Certificate{}, err
		}
		if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
			return tls.Certificate{}, err
		}
		return tls.X509KeyPair(certPEM, keyPEM)
	}
	certPEM, keyPEM, err := generate()
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

func generate() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "fabric-emulator"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              Hosts,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tpl, &tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}
