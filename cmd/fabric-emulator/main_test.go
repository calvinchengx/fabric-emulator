package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"FABRIC_ADDR", "FABRIC_DATA_DIR", "FABRIC_ENTRA_ISSUER",
		"FABRIC_ENTRA_JWKS_URL", "FABRIC_ENTRA_TLS_INSECURE", "FABRIC_DISABLE_TLS"} {
		t.Setenv(k, "")
	}
}

func TestRunErrors(t *testing.T) {
	clearEnv(t)
	if err := run([]string{"-bogus-flag"}, nil); err == nil {
		t.Fatal("unknown flag accepted")
	}
	if err := run(nil, nil); err == nil {
		t.Fatal("missing issuer accepted")
	}
	if err := run([]string{"-entra-issuer", "https://x/t/v2.0", "-addr", "999.999.999.999:1"}, nil); err == nil {
		t.Fatal("unlistenable addr accepted")
	}
}

// poll waits for the health endpoint to answer.
func poll(t *testing.T, client *http.Client, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("health never came up at %s", url)
}

func TestRunServesTLS(t *testing.T) {
	clearEnv(t)
	port := freePort(t)
	dir := t.TempDir()
	// Stop the server and wait for run to return before TempDir cleanup:
	// the store must release the database file first (Windows cannot
	// delete a file that is still open).
	stop, done := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(stop); <-done })
	go func() {
		defer close(done)
		_ = run([]string{
			"-entra-issuer", "https://127.0.0.1:1/t/v2.0", // JWKS unreachable is fine: /health needs no token
			"-addr", fmt.Sprintf("127.0.0.1:%d", port),
			"-data-dir", dir,
		}, stop)
	}()
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	poll(t, client, fmt.Sprintf("https://127.0.0.1:%d/health", port))
	// An authenticated route without a token is a Fabric-shaped 401.
	resp, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/v1/workspaces", port))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /v1 = %d; want 401", resp.StatusCode)
	}
}

func TestRunServesPlainHTTP(t *testing.T) {
	clearEnv(t)
	port := freePort(t)
	stop, done := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(stop); <-done })
	go func() {
		defer close(done)
		_ = run([]string{
			"-entra-issuer", "https://127.0.0.1:1/t/v2.0",
			"-addr", fmt.Sprintf("127.0.0.1:%d", port),
			"-disable-tls",
		}, stop)
	}()
	poll(t, http.DefaultClient, fmt.Sprintf("http://127.0.0.1:%d/health", port))
}

func TestRunDataDirAndTLSFailures(t *testing.T) {
	clearEnv(t)
	// -data-dir pointing at an existing FILE: MkdirAll fails.
	dir := t.TempDir()
	file := dir + "/occupied"
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"-entra-issuer", "https://x/t/v2.0", "-addr", "127.0.0.1:0", "-data-dir", file}, nil)
	if err == nil {
		t.Fatal("data-dir-is-a-file accepted")
	}
	// tls subpath blocked: data dir ok, cert persistence fails.
	dir3 := t.TempDir()
	if err := os.WriteFile(dir3+"/tls", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-entra-issuer", "https://x/t/v2.0", "-addr", "127.0.0.1:0", "-data-dir", dir3}, nil); err == nil {
		t.Fatal("broken tls dir accepted")
	}
}
