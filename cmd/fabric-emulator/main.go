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
	if err := run(os.Args[1:], nil, nil); err != nil {
		log.Fatal(err)
	}
}

// run serves until the process exits, or until stop closes (nil = never).
// Tests stop the server so the store releases the database file before
// TempDir cleanup — Windows cannot delete a file that is still open.
//
// ready (nil = don't report) receives the address the HTTP listener actually
// bound, just before serving begins. Callers that ask for :0 learn their port
// from here; reserving one up front and passing it in races every other
// listener on the machine for the window it takes to open the store and
// generate a certificate.
func run(args []string, stop <-chan struct{}, ready chan<- net.Addr) error {
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
	fs.BoolVar(&cfg.TSQLStrict, "tsql-strict", cfg.TSQLStrict, "refuse T-SQL that real Fabric rejects but SQL Server accepts (recursive CTEs, triggers, enforced constraints; see docs/29)")
	fs.StringVar(&cfg.WarehouseSQLURL, "warehouse-sql-url", cfg.WarehouseSQLURL, "real SQL Server backend the SQL endpoint relays to (go-mssqldb DSN; empty = stub result)")
	fs.StringVar(&cfg.AirflowURL, "airflow-url", cfg.AirflowURL, "Apache Airflow 2.10 REST API base URL (empty = off)")
	fs.StringVar(&cfg.AirflowDAGDir, "airflow-dag-dir", cfg.AirflowDAGDir, "shared Airflow DAG directory")
	fs.StringVar(&cfg.AirflowUsername, "airflow-username", cfg.AirflowUsername, "Airflow basic-auth username")
	fs.StringVar(&cfg.AirflowPassword, "airflow-password", cfg.AirflowPassword, "Airflow basic-auth password")
	fs.StringVar(&cfg.MLflowURL, "mlflow-url", cfg.MLflowURL, "MLflow tracking/model-registry server URL (empty = off)")
	fs.StringVar(&cfg.KQLURL, "kql-url", cfg.KQLURL, "real Kusto engine the Eventhouse/KQL Database surface relays to (e.g. http://kustainer:8080; empty = 501)")
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
	if ready != nil {
		// Non-blocking: an unread channel must not wedge the server.
		select {
		case ready <- ln.Addr():
		default:
		}
	}
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
