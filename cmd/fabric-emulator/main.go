// Command fabric-emulator runs the Microsoft Fabric control-plane emulator.
// It validates bearer tokens against an Entra issuer (entra-emulator or a
// real tenant) and serves the /v1 workspace/item/RBAC/LRO surface.
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/calvinchengx/fabric-emulator/internal/config"
	"github.com/calvinchengx/fabric-emulator/internal/server"
	"github.com/calvinchengx/fabric-emulator/internal/tlscert"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	cfg := config.FromEnvPartial()
	fs := flag.NewFlagSet("fabric-emulator", flag.ContinueOnError)
	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "listen address")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "state directory (empty = in-memory)")
	fs.StringVar(&cfg.EntraIssuer, "entra-issuer", cfg.EntraIssuer, "trusted Entra issuer URL (required)")
	fs.StringVar(&cfg.EntraJWKSURL, "entra-jwks-url", cfg.EntraJWKSURL, "JWKS URL (derived from issuer when empty)")
	fs.BoolVar(&cfg.EntraTLSInsecure, "entra-tls-insecure", cfg.EntraTLSInsecure, "skip TLS verification fetching JWKS")
	fs.Int64Var(&cfg.LRODelaySeconds, "lro-delay", cfg.LRODelaySeconds, "virtual seconds operations stay Running")
	fs.BoolVar(&cfg.DisableTLS, "disable-tls", cfg.DisableTLS, "serve plain HTTP")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := cfg.Finish(); err != nil {
		return err
	}

	srv, err := server.New(cfg, nil)
	if err != nil {
		return err
	}
	defer srv.Close()

	if cfg.DataDir != "" {
		if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
			return err
		}
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	scheme := "https"
	if cfg.DisableTLS {
		scheme = "http"
	} else {
		cert, err := tlscert.Load(cfg.DataDir)
		if err != nil {
			return err
		}
		ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})
	}
	fmt.Printf("fabric-emulator listening on %s://%s (issuer: %s)\n", scheme, ln.Addr(), cfg.EntraIssuer)
	return http.Serve(ln, srv.Handler())
}
