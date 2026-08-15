// Package config resolves the emulator's runtime configuration from
// environment variables (FABRIC_*) with flag overrides applied by cmd. The
// docker-compose contract (FABRIC_ENTRA_ISSUER, FABRIC_ENTRA_JWKS_URL,
// FABRIC_ENTRA_TLS_INSECURE) is the canonical wiring to entra-emulator.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the resolved emulator configuration.
type Config struct {
	// Addr is the listen address, e.g. ":9443".
	Addr string
	// DataDir holds SQLite and TLS state. Empty means in-memory DB and
	// ephemeral TLS keys.
	DataDir string

	// EntraIssuer is the exact iss expected in bearer tokens, e.g.
	// https://entra-emulator:8443/{tenant}/v2.0 — or a real Entra issuer.
	EntraIssuer string
	// EntraJWKSURL is where signing keys are fetched. Derived from
	// EntraIssuer when unset ({issuer minus /v2.0}/discovery/v2.0/keys).
	EntraJWKSURL string
	// AKVVaultHost is the one non-Azure host:port a vault secret reference
	// may resolve against — the family's keyvault-emulator, e.g.
	// "keyvault-emulator:8444". Azure's own *.vault.azure.net (and the
	// sovereign suffixes) are always allowed; everything else is refused,
	// because ResolveSecret sends a vault-audience bearer token to that host.
	AKVVaultHost string

	// EntraTLSInsecure skips TLS verification when fetching JWKS — for the
	// compose network where entra-emulator serves a self-signed cert.
	EntraTLSInsecure bool

	// LRODelaySeconds is the virtual time an async operation stays Running
	// before it succeeds. 0 = completes on the next poll.
	LRODelaySeconds int64
	// RetryAfterSeconds is advertised in 202 Retry-After headers.
	RetryAfterSeconds int

	// DisableTLS serves plain HTTP (useful behind a TLS-terminating proxy
	// or for curl-based exploration). Default is self-signed TLS, matching
	// entra-emulator.
	DisableTLS bool

	// SparkLivyURL, when set, is a real Apache Livy backend the emulator
	// reverse-proxies its Livy endpoint to (and, opt-in, runs RunNotebook
	// jobs against). Empty leaves the Livy routes 501 and jobs on the
	// deterministic clock.
	SparkLivyURL string

	// SparkAgentURL, when set, makes the emulator terminate the Livy protocol
	// itself and drive a Spark statement-executor agent (real interactive
	// sessions, no Apache Livy server). Takes precedence over SparkLivyURL for
	// the interactive session/statement path. See e2e/livy.
	//
	// It also makes RunNotebook jobs EXECUTE. Without an agent the emulator
	// parses a notebook and waits for an external engine to report — the right
	// contract, but one no published artifact could satisfy, so a consumer's
	// notebook job hung forever. With an agent the emulator is the pool: it runs
	// the cells and reports the same results a Spark pool would post. See
	// internal/api/notebookdrive.go and e2e/notebook-driven.
	SparkAgentURL string

	// TerminalURL, when set (e.g. "http://ttyd:7681"), lets the Flow view open a
	// terminal pane proxied through the portal's origin. Empty = the feature
	// does not exist: no route is mounted and the pane never appears.
	//
	// THE PORTAL IS OTHERWISE UNAUTHENTICATED, and deliberately so. A terminal
	// is not another read; it is arbitrary execution. So the proxy route carries
	// its own bearer (TerminalToken) rather than inheriting the portal's
	// premise, and the other routes are untouched.
	//
	// THAT PREMISE IS NARROWER THAN IT LOOKS. The portal is 11 GETs plus
	// `POST /_emulator/portal/models/{id}/query`, which runs caller-supplied DAX
	// through the same evaluator as executeQueries (internal/server/portal.go).
	// Three consequences the read-only GETs did not carry:
	//
	//  1. Anyone who reaches the port can evaluate arbitrary DAX against any
	//     import model — caller input into an evaluator, not a fixed route.
	//  2. It is not only a read: publishQuery fires, so the request puts a
	//     visible event in every other viewer's flow log.
	//  3. Arbitrary DAX is unbounded compute. Cheap to limit now, awkward to
	//     retrofit if the portal is ever reachable beyond localhost.
	//
	// Fine for a local emulator, and not an argument against the runner. Written
	// down so the next feature here is weighed rather than waved through.
	TerminalURL string

	// TerminalToken authorises the terminal proxy. Generated at startup when
	// TerminalURL is set and this is empty, and printed once — the operator
	// pastes it into the pane. It is NOT served by any portal endpoint: the
	// portal is reachable by anyone who can reach the port, so handing the
	// token out there would be the same as having none.
	TerminalToken string

	// TenantAdmins are the principal ids (oid, or appid for a service
	// principal) that count as Fabric administrators for /v1/admin/*.
	// Comma-separated in FABRIC_TENANT_ADMINS. Empty means nobody is one, so
	// every mutating admin call is refused — see internal/api/tenantadmin.go.
	TenantAdmins []string

	// SQLTDSAddr, when set (e.g. ":1433"), starts the warehouse SQL endpoint:
	// a TDS listener that terminates Entra FedAuth and answers T-SQL. Empty
	// leaves the SQL endpoint off. See docs/16-warehouse-tds.md.
	SQLTDSAddr string

	// WarehouseSQLURL, when set, is a real SQL Server backend (go-mssqldb DSN)
	// the SQL endpoint relays authenticated queries to. Empty leaves the
	// endpoint answering the T1 stub result.
	WarehouseSQLURL string

	// ListPageSize is the server's page size for list APIs. 0 uses the default
	// (api.DefaultListPageSize); a small value forces every client through the
	// continuation-token loop, which is how you prove a client handles
	// pagination before real data grows into it. Negative disables paging.
	ListPageSize int

	// ForceLRO makes every API that documents BOTH a synchronous and an
	// asynchronous outcome answer the ASYNCHRONOUS one — FABRIC_FORCE_LRO=1.
	// Same purpose as a small ListPageSize: force every client through a path
	// real Fabric WILL take, before a real tenant is the thing that finds out.
	//
	// This began as FABRIC_DEFINITION_LRO for getDefinition alone, and that
	// name still works. It was generalised on measuring a SECOND instance the
	// same morning, which makes it a class rather than a case:
	//
	//   getDefinition — reference documents 200 and 202; a real tenant answered
	//     202. A client reading the 202 body gets `null` and reports an EMPTY
	//     definition rather than an error.
	//   createItem — reference documents 201 and 202 ("This API supports long
	//     running operations"); a real tenant answered 202 for a Warehouse
	//     create, and a client indexing the body got `None["id"]`. The emulator
	//     could NEVER produce that shape: it went async only when a definition
	//     was supplied, and Create Warehouse "does not support create a
	//     warehouse with definition" — so the one type measured as async was
	//     the one type guaranteed to be synchronous here.
	//   git/initializeConnection — reference documents 200 and 202 and says it
	//     supports LRO; the emulator answered 200 unconditionally. Its async
	//     result carries a real body, unlike commitToGit/updateFromGit.
	//
	// SCOPE, stated so nobody reads more into the name than is there: these
	// three are the surfaces AUDITED against the reference, not a proof that
	// no fourth exists. Already-async surfaces (updateDefinition, commitToGit,
	// updateFromGit, assign/unassignToCapacity, provision/deprovision
	// identity, deploy, job instances) were confirmed async and needed
	// nothing. Two documented LROs are NOT implemented at all and so are out of
	// scope here rather than fixed: Load Table and sqlEndpoints refreshMetadata.
	//
	// Off by default: the synchronous answers are equally legal and are what
	// most calls see. The point is that the other half is reachable at all.
	ForceLRO bool

	// NameReservation holds a DELETED item's display name for this long before
	// it can be reused — FABRIC_NAME_RESERVATION=30s. Zero (the default) frees
	// the name immediately.
	//
	// Measured on a tenant 2026-08-11: delete a Notebook and recreate it under
	// the same name and Fabric answers `409 ItemDisplayNameNotAvailableYet`,
	// `isRetriable: true`, with the name free again ~20s later. The emulator
	// freed it instantly, so a provision/teardown/re-provision loop passed here
	// forever and failed on a real deploy — the permissive direction.
	//
	// A DURATION rather than a bool, because the tenant's own message
	// ("the upcoming minutes") disagreed with its own behaviour (~20 seconds).
	// Nothing observed licenses a constant, so the window is the operator's to
	// choose and the emulator states no opinion about how long Fabric waits.
	NameReservation time.Duration

	// WebActivityStub makes pipeline Web activities record success WITHOUT
	// calling anything — FABRIC_WEB_ACTIVITY=stub. Off by default: a Web
	// activity that fabricates a response is a false pass, and a pipeline
	// branching on the body would behave differently in Fabric. On for a
	// hermetic CI leg that must not reach the network.
	WebActivityStub bool

	// CustomActivityShell enables the Azure Batch (`Custom`) pipeline activity,
	// which runs a caller-supplied SHELL COMMAND in the Spark agent's container.
	// On by default: a notebook cell on that same agent can already
	// subprocess.run, behind the same bearer and workspace RBAC, so refusing
	// Custom by default was the false pass for any pipeline that actually uses
	// Batch — the same reversal as Web (FABRIC_WEB_ACTIVITY=stub).
	//
	// FABRIC_CUSTOM_ACTIVITY=off restores the refusal so no command reaches the
	// agent. The old opt-in spelling `shell` remains on. The portal terminal
	// stays opt-in: that route is still arbitrary execution on an otherwise-
	// unauthenticated surface.
	CustomActivityShell bool

	// TSQLStrict refuses T-SQL that the SQL Server sidecar accepts but real
	// Fabric rejects — recursive CTEs, triggers, enforced constraints and the
	// rest of docs/29-tsql-parity.md's Class B. Off by default because it
	// *removes* capability: it makes a locally green build mean a
	// Fabric-green build, at the cost of failing SQL that works today.
	TSQLStrict bool

	// AirflowURL attaches an upstream Apache Airflow 2.10 REST API. DAG files
	// are materialised into AirflowDAGDir, which must be a shared volume mounted
	// as the scheduler's DAG folder.
	AirflowURL      string
	AirflowDAGDir   string
	AirflowUsername string
	AirflowPassword string

	// MLflowURL attaches a real MLflow tracking and model-registry server.
	MLflowURL string

	// KQLURL, when set, is a real Kusto engine (Microsoft's kustainer
	// container, or any ADX/Eventhouse cluster) that the Real-Time
	// Intelligence surface relays queries, management commands, and inline
	// ingestion to. The emulator still terminates Fabric's own contract —
	// bearer validation, workspace RBAC, and per-KQL-database isolation —
	// and only the KQL itself executes upstream. Empty leaves the Kusto
	// routes answering an honest 501. See docs/25-rti-kusto.md.
	KQLURL string

	// KafkaBootstrap, when set, is an Apache Kafka broker (host:port) that
	// Eventstream items provision topics on and that the JVM Spark adapter
	// subscribes to. Empty leaves Eventstream create as management-only and
	// Spark / Custom-source execution answering an honest 501.
	// See docs/51-eventstream-kafka.md.
	KafkaBootstrap string

	// ARMURL is arm-emulator's origin. When set, Microsoft.Fabric/capacities
	// created over ARM appear on GET /v1/capacities. Empty keeps the seeded
	// local capacity only — standalone fabric-cicd still works.
	ARMURL string
	// ARMPollSeconds is how often the ARM capacities feed is refreshed.
	ARMPollSeconds int

	// Version and Commit are stamped at build time — `-ldflags -X main.version`
	// and `-X main.commit`, set by GoReleaser for the binaries and by a build
	// arg for the image. Empty in a plain `go build`, which is honest: a source
	// build has no release identity to claim.
	Version string
	Commit  string
}

// FromEnv builds a validated Config from FABRIC_* environment variables.
func FromEnv() (*Config, error) {
	c := FromEnvPartial()
	return c, c.Finish()
}

// FromEnvPartial reads the environment without validating — cmd applies flag
// overrides first, then calls Finish.
func FromEnvPartial() *Config {
	return &Config{
		Addr:             envOr("FABRIC_ADDR", ":9443"),
		DataDir:          envDefault("FABRIC_DATA_DIR", DefaultDataDir),
		EntraIssuer:      os.Getenv("FABRIC_ENTRA_ISSUER"),
		EntraJWKSURL:     os.Getenv("FABRIC_ENTRA_JWKS_URL"),
		EntraTLSInsecure: boolEnv("FABRIC_ENTRA_TLS_INSECURE"),
		AKVVaultHost:     os.Getenv("FABRIC_AKV_VAULT_HOST"),
		DisableTLS:       boolEnv("FABRIC_DISABLE_TLS"),
		SparkLivyURL:     os.Getenv("FABRIC_SPARK_LIVY_URL"),
		SparkAgentURL:    os.Getenv("FABRIC_SPARK_AGENT_URL"),
		SQLTDSAddr:       os.Getenv("FABRIC_SQL_TDS_ADDR"),
		TerminalURL:      os.Getenv("FABRIC_TERMINAL_URL"),
		TerminalToken:    os.Getenv("FABRIC_TERMINAL_TOKEN"),
		TenantAdmins:     splitList(os.Getenv("FABRIC_TENANT_ADMINS")),
		// FABRIC_DEFINITION_LRO is the older, narrower name this shipped under
		// and is still honoured, so a compose file or CI leg that already sets
		// it keeps working rather than silently losing the async path.
		ForceLRO:            boolEnv("FABRIC_FORCE_LRO") || boolEnv("FABRIC_DEFINITION_LRO"),
		NameReservation:     durationEnv("FABRIC_NAME_RESERVATION"),
		WebActivityStub:     strings.EqualFold(os.Getenv("FABRIC_WEB_ACTIVITY"), "stub"),
		CustomActivityShell: customActivityEnabled(os.Getenv("FABRIC_CUSTOM_ACTIVITY")),
		WarehouseSQLURL:     os.Getenv("FABRIC_WAREHOUSE_SQL_URL"),
		TSQLStrict:          boolEnv("FABRIC_TSQL_STRICT"),
		ListPageSize:        intEnv("FABRIC_LIST_PAGE_SIZE"),
		AirflowURL:          os.Getenv("FABRIC_AIRFLOW_URL"),
		AirflowDAGDir:       os.Getenv("FABRIC_AIRFLOW_DAG_DIR"),
		AirflowUsername:     os.Getenv("FABRIC_AIRFLOW_USERNAME"),
		AirflowPassword:     os.Getenv("FABRIC_AIRFLOW_PASSWORD"),
		MLflowURL:           os.Getenv("FABRIC_MLFLOW_URL"),
		KQLURL:              os.Getenv("FABRIC_KQL_URL"),
		KafkaBootstrap:      os.Getenv("FABRIC_KAFKA_BOOTSTRAP"),
		ARMURL:              os.Getenv("FABRIC_ARM_URL"),
		ARMPollSeconds:      intEnv("FABRIC_ARM_POLL_SECONDS"),
		RetryAfterSeconds:   1,
	}
}

// Finish validates and derives dependent fields. Call after flag overrides.
func (c *Config) Finish() error {
	if c.EntraIssuer == "" {
		return fmt.Errorf("FABRIC_ENTRA_ISSUER is required: the issuer bearer tokens must carry (an entra-emulator or real Entra v2.0 issuer URL)")
	}
	if c.EntraJWKSURL == "" {
		c.EntraJWKSURL = DeriveJWKSURL(c.EntraIssuer)
	}
	if c.RetryAfterSeconds <= 0 {
		c.RetryAfterSeconds = 1
	}
	// A terminal without a token is a shell with no lock on it, so the token is
	// generated rather than defaulted: there is no safe fixed value, and an
	// empty one is refused by the proxy anyway. Set FABRIC_TERMINAL_TOKEN only
	// to pin it for a scripted demo.
	if c.TerminalURL != "" && c.TerminalToken == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("generating the terminal token: %w", err)
		}
		c.TerminalToken = hex.EncodeToString(b)
	}
	return nil
}

// DeriveJWKSURL maps a v2.0 issuer to its JWKS endpoint using the Entra
// convention: {origin}/{tenant}/v2.0 → {origin}/{tenant}/discovery/v2.0/keys.
// Issuers not ending in /v2.0 get /discovery/v2.0/keys appended.
func DeriveJWKSURL(issuer string) string {
	base := strings.TrimSuffix(issuer, "/")
	base = strings.TrimSuffix(base, "/v2.0")
	return base + "/discovery/v2.0/keys"
}

// DefaultDataDir is where state lands when FABRIC_DATA_DIR is not set at all.
// The family persists by default: an emulator that forgets its workspaces and
// OneLake contents on restart is a surprise.
const DefaultDataDir = "./data"

// envDefault distinguishes UNSET from SET-EMPTY, which envOr cannot: unset
// takes the default, while an explicit empty value is honoured as empty. For
// DataDir that is the difference between persisting and running in memory,
// and the compose files use the empty form so a throwaway stack leaves no
// SQLite file in a container layer about to be deleted.
func envDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// durationEnv reads a Go duration ("30s", "2m"), zero when unset or
// unparseable. A bad value reads as OFF rather than as a default window: a
// silent fallback to some non-zero duration would make every create/delete
// loop mysteriously fail on a typo.
func durationEnv(key string) time.Duration {
	d, err := time.ParseDuration(os.Getenv(key))
	if err != nil {
		return 0
	}
	return d
}

func boolEnv(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// customActivityEnabled is on unless the value is the explicit refuse.
// Empty and "shell" (the old opt-in spelling) are on; "off" matches
// FABRIC_WEB_ACTIVITY=stub as the hermetic escape.
func customActivityEnabled(v string) bool {
	return !strings.EqualFold(strings.TrimSpace(v), "off")
}

// intEnv reads an integer environment variable, 0 when unset or unparseable.
func intEnv(key string) int {
	n, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return 0
	}
	return n
}

// Build is how this emulator names itself: `v0.15.2-a1b2c3d`.
//
// WHY IT IS COMPOSED HERE and not at each call site. It is read from at least
// three places — the `version` subcommand, /health, and the portal's top bar —
// and the point of the string is that a screenshot, a recording and a bug
// report all quote the SAME identity. Three formatters would eventually
// disagree about a prefix or a separator, and then two reports of one build
// would look like reports of two.
//
// The `v` is normalised because the two build paths disagree at the source:
// GoReleaser stamps `0.15.2` while the image build passes `${{ github.ref_name }}`,
// which is `v0.15.2`. Stripping and re-adding makes the output independent of
// which one produced the binary.
//
// An unstamped build says `dev` rather than inventing a version, and a build
// with no commit says just the version: an empty suffix reads as a bug in this
// function rather than as the absence of a hash.
func (c *Config) Build() string {
	v := strings.TrimPrefix(c.Version, "v")
	if v == "" || v == "dev" {
		v = "dev"
	} else {
		v = "v" + v
	}
	if c.Commit == "" {
		return v
	}
	// Short form: a reader quoting a hash back from a screenshot will not type
	// forty characters, and seven is what git itself abbreviates to.
	short := c.Commit
	if len(short) > 7 {
		short = short[:7]
	}
	return v + "-" + short
}

// splitList parses a comma-separated env var, trimming spaces and dropping
// empties, so "a, b," yields two ids rather than three with one blank — a
// blank id would otherwise match a principal whose ID failed to parse.
// SplitList is exported for cmd, which parses the same list from a flag.
func SplitList(s string) []string { return splitList(s) }

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}
