package main

import (
	"crypto/tls"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// serve runs the emulator on an ephemeral port and returns the address it
// actually bound. Both details matter, and both come from a Windows-only
// flake that failed roughly one CI run in nine:
//
//   - run's error is surfaced. It used to be discarded, which made a bind
//     collision and a slow start indistinguishable: every failure, whatever
//     its cause, reported only that health never came up.
//   - the port comes from run rather than from a listener we open and close
//     beforehand. Reserving one up front leaves a window — widened by opening
//     the store and generating a certificate — for anything else binding :0
//     to take it first.
//
// Neither of those is the budget, and the budget is what failed next: PR #303
// (job 95436375580, a PYTHON-ONLY diff) reported "run never reported a listen
// address" on windows-latest after 60s, with the server's own
// `listening on https://127.0.0.1:55822` printed in the same log — it came up,
// just later than the wait. That runner was contended enough that
// `internal/store` took 93s and `internal/api` 244s, so the 60s was measuring
// the runner rather than the code.
//
// So the budget follows poll()'s reasoning below, which the same file already
// states and which was never extended up here: the select returns the instant
// `ready` fires, so a fast machine pays NOTHING for the headroom, while a slow
// one stops reporting a startup as a hang. The failure mode a longer wait
// costs us is a genuine deadlock taking longer to surface — bounded, and
// `case <-done` still catches the common case of run exiting early.
//
// 120s rather than a rounder, larger number, because there IS a ceiling: two
// tests call serve(), and `go test` gives the package 10 minutes by default,
// so a budget of 300s each would replace a clean "never reported a listen
// address" with a whole-package timeout panic — a worse message for the same
// event. 120s is double the wait that lapsed, and 240s worst case.
func serve(t *testing.T, args ...string) string {
	t.Helper()
	stop, done, ready := make(chan struct{}), make(chan struct{}), make(chan net.Addr, 1)
	var runErr error
	go func() {
		defer close(done)
		runErr = run(append(args, "-addr", "127.0.0.1:0"), stop, ready)
	}()
	// Stop the server and wait for run to return before TempDir cleanup: the
	// store must release the database file first (Windows cannot delete a
	// file that is still open).
	t.Cleanup(func() { close(stop); <-done })
	select {
	case addr := <-ready:
		return addr.String()
	case <-done:
		t.Fatalf("run exited before it began serving: %v", runErr)
	case <-time.After(120 * time.Second):
		t.Fatal("run never reported a listen address")
	}
	return ""
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"FABRIC_ADDR", "FABRIC_DATA_DIR", "FABRIC_ENTRA_ISSUER",
		"FABRIC_ENTRA_JWKS_URL", "FABRIC_ENTRA_TLS_INSECURE", "FABRIC_DISABLE_TLS",
		"FABRIC_ARM_URL", "FABRIC_ARM_POLL_SECONDS"} {
		t.Setenv(k, "")
	}
}

func TestRunErrors(t *testing.T) {
	clearEnv(t)
	if err := run([]string{"-bogus-flag"}, nil, nil); err == nil {
		t.Fatal("unknown flag accepted")
	}
	if err := run(nil, nil, nil); err == nil {
		t.Fatal("missing issuer accepted")
	}
	if err := run([]string{"-entra-issuer", "https://x/t/v2.0", "-addr", "999.999.999.999:1"}, nil, nil); err == nil {
		t.Fatal("unlistenable addr accepted")
	}
}

// TestRunReportsNoAddressWhenListenFails: run announces an address only once
// it has one. A report on the failure path would strand serve() waiting on a
// health check for a server that already exited — the exact misdiagnosis this
// channel exists to prevent.
func TestRunReportsNoAddressWhenListenFails(t *testing.T) {
	clearEnv(t)
	ready := make(chan net.Addr, 1)
	err := run([]string{"-entra-issuer", "https://x/t/v2.0", "-addr", "999.999.999.999:1"}, nil, ready)
	if err == nil {
		t.Fatal("unlistenable addr accepted")
	}
	select {
	case addr := <-ready:
		t.Fatalf("reported %v after failing to listen", addr)
	default:
	}
}

// poll waits for the health endpoint to answer. The budget is generous
// because a contended Windows runner can spend seconds opening the store and
// generating a certificate before Serve; polling returns the moment health
// answers, so a fast machine pays nothing for the headroom. The client must
// carry its own timeout — an unbounded Get can outlive the deadline on its
// own and turn a slow handshake into a missed budget.
func poll(t *testing.T, client *http.Client, url string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
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
	addr := serve(t,
		"-entra-issuer", "https://127.0.0.1:1/t/v2.0", // JWKS unreachable is fine: /health needs no token
		"-data-dir", t.TempDir())
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	poll(t, client, "https://"+addr+"/health")
	// An authenticated route without a token is a Fabric-shaped 401.
	resp, err := client.Get("https://" + addr + "/v1/workspaces")
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
	addr := serve(t, "-entra-issuer", "https://127.0.0.1:1/t/v2.0", "-disable-tls")
	poll(t, &http.Client{Timeout: 5 * time.Second}, "http://"+addr+"/health")
}

func TestRunDataDirAndTLSFailures(t *testing.T) {
	clearEnv(t)
	// -data-dir pointing at an existing FILE: MkdirAll fails.
	dir := t.TempDir()
	file := dir + "/occupied"
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"-entra-issuer", "https://x/t/v2.0", "-addr", "127.0.0.1:0", "-data-dir", file}, nil, nil)
	if err == nil {
		t.Fatal("data-dir-is-a-file accepted")
	}
	// tls subpath blocked: data dir ok, cert persistence fails.
	dir3 := t.TempDir()
	if err := os.WriteFile(dir3+"/tls", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-entra-issuer", "https://x/t/v2.0", "-addr", "127.0.0.1:0", "-data-dir", dir3}, nil, nil); err == nil {
		t.Fatal("broken tls dir accepted")
	}
}
