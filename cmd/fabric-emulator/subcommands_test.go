package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVersionSubcommand(t *testing.T) {
	clearEnv(t)
	if err := run([]string{"version"}); err != nil {
		t.Fatalf("version: %v", err)
	}
}

func TestHealthcheckSubcommand(t *testing.T) {
	clearEnv(t)

	// Healthy plain-HTTP instance (exercises the https→http fallback).
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer okSrv.Close()
	t.Setenv("FABRIC_ADDR", strings.TrimPrefix(okSrv.URL, "http://"))
	if err := run([]string{"healthcheck"}); err != nil {
		t.Fatalf("healthcheck against healthy instance: %v", err)
	}

	// Healthy TLS instance with a self-signed cert (the container case).
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer tlsSrv.Close()
	if err := healthcheck(strings.TrimPrefix(tlsSrv.URL, "https://")); err != nil {
		t.Fatalf("healthcheck against TLS instance: %v", err)
	}

	// Unhealthy: listening but not OK.
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer badSrv.Close()
	if err := healthcheck(strings.TrimPrefix(badSrv.URL, "http://")); err == nil {
		t.Fatal("503 instance reported healthy")
	}

	// Nothing listening.
	if err := healthcheck(fmt.Sprintf("127.0.0.1:%d", freePort(t))); err == nil {
		t.Fatal("dead instance reported healthy")
	}

	// Malformed address.
	if err := healthcheck("not-an-addr"); err == nil {
		t.Fatal("malformed addr accepted")
	}

	// Empty host defaults to loopback (the ":9443" container default).
	if err := healthcheck(fmt.Sprintf(":%d", freePort(t))); err == nil {
		t.Fatal("dead loopback instance reported healthy")
	}
}
