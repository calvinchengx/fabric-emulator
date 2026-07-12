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
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/config"
	"github.com/calvinchengx/fabric-emulator/internal/server"
	"github.com/calvinchengx/fabric-emulator/internal/tlscert"
)

// version is stamped by GoReleaser via -ldflags "-X main.version=…".
var version = "dev"

func main() {
	if err := run(os.Args[1:], nil); err != nil {
		log.Fatal(err)
	}
}

// run serves until the process exits, or until stop closes (nil = never).
// Tests stop the server so the store releases the database file before
// TempDir cleanup — Windows cannot delete a file that is still open.
func run(args []string, stop <-chan struct{}) error {
	cfg := config.FromEnvPartial()
	if len(args) > 0 {
		switch args[0] {
		case "version":
			fmt.Println("fabric-emulator", version)
			return nil
		case "healthcheck":
			return healthcheck(cfg.Addr)
		}
	}
	fs := flag.NewFlagSet("fabric-emulator", flag.ContinueOnError)
	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "listen address")
	fs.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "state directory (empty = in-memory)")
	fs.StringVar(&cfg.EntraIssuer, "entra-issuer", cfg.EntraIssuer, "trusted Entra issuer URL (required)")
	fs.StringVar(&cfg.EntraJWKSURL, "entra-jwks-url", cfg.EntraJWKSURL, "JWKS URL (derived from issuer when empty)")
	fs.BoolVar(&cfg.EntraTLSInsecure, "entra-tls-insecure", cfg.EntraTLSInsecure, "skip TLS verification fetching JWKS")
	fs.Int64Var(&cfg.LRODelaySeconds, "lro-delay", cfg.LRODelaySeconds, "virtual seconds operations stay Running")
	fs.BoolVar(&cfg.DisableTLS, "disable-tls", cfg.DisableTLS, "serve plain HTTP")
	fs.StringVar(&cfg.SparkLivyURL, "spark-livy-url", cfg.SparkLivyURL, "real Apache Livy backend for the Livy passthrough (empty = 501)")
	fs.StringVar(&cfg.SparkAgentURL, "spark-agent-url", cfg.SparkAgentURL, "Spark statement-executor agent for native Livy sessions (empty = off)")
	fs.StringVar(&cfg.SQLTDSAddr, "sql-tds-addr", cfg.SQLTDSAddr, "listen address for the warehouse SQL/TDS endpoint (e.g. :1433; empty = off)")
	fs.StringVar(&cfg.WarehouseSQLURL, "warehouse-sql-url", cfg.WarehouseSQLURL, "real SQL Server backend the SQL endpoint relays to (go-mssqldb DSN; empty = stub result)")
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
	if stop != nil {
		go func() {
			<-stop
			ln.Close()
		}()
	}
	// The warehouse SQL/TDS endpoint runs on its own TCP listener (a raw binary
	// protocol, not HTTP). It terminates Entra FedAuth against the same issuer.
	if srv.TDS != nil && cfg.SQLTDSAddr != "" {
		tln, err := net.Listen("tcp", cfg.SQLTDSAddr)
		if err != nil {
			return err
		}
		if stop != nil {
			go func() { <-stop; tln.Close() }()
		}
		fmt.Printf("fabric-emulator SQL/TDS endpoint listening on %s\n", tln.Addr())
		go func() { _ = srv.TDS.Serve(tln) }()
	}

	fmt.Printf("fabric-emulator listening on %s://%s (issuer: %s)\n", scheme, ln.Addr(), cfg.EntraIssuer)
	return http.Serve(ln, srv.Handler())
}

// healthcheck probes /health on the local instance and exits 0 when healthy —
// distroless images have no shell, so container HEALTHCHECKs exec this binary.
// The self-signed cert isn't in any trust store; this is a localhost liveness
// probe, so skipping verification is fine.
func healthcheck(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" {
		host = "127.0.0.1"
	}
	client := &http.Client{
		Timeout:   3 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err := client.Get("https://" + net.JoinHostPort(host, port) + "/health")
	if err != nil {
		// TLS may be disabled; retry plain HTTP before giving up.
		if resp, err = client.Get("http://" + net.JoinHostPort(host, port) + "/health"); err != nil {
			return err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health: %s", resp.Status)
	}
	return nil
}
