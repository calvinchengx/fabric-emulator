#!/usr/bin/env bash
# Microsoft's Fabric CLI (fab) drives the emulator's control plane — the highest-
# authority borrowed oracle. fab hardcodes https:// and the MSAL authority
# login.microsoftonline.com, so entra-emulator IS that host (compose alias) and
# fabric is api.fabric.microsoft.com; both self-signed certs go into a CA bundle.
set -uo pipefail
ENTRA=login.microsoftonline.com:443
FABRIC=api.fabric.microsoft.com:443
WS=cliws.Workspace
CAP="Emulator Capacity"

wait_tls() { local hp=$1 h=${1%:*}; for i in $(seq 1 90); do
  openssl s_client -connect "$hp" -servername "$h" </dev/null 2>/dev/null | grep -q "BEGIN CERTIFICATE" && return 0; sleep 1; done; return 1; }
fail() { echo "FABRIC-CLI E2E: FAIL ($1)"; exit 1; }

echo "==> waiting for entra + fabric TLS"; wait_tls "$ENTRA" && wait_tls "$FABRIC" || fail "servers never came up"
openssl s_client -connect "$ENTRA"  -servername login.microsoftonline.com </dev/null 2>/dev/null | openssl x509 >  /tmp/ca.pem
openssl s_client -connect "$FABRIC" -servername api.fabric.microsoft.com   </dev/null 2>/dev/null | openssl x509 >> /tmp/ca.pem
export REQUESTS_CA_BUNDLE=/tmp/ca.pem SSL_CERT_FILE=/tmp/ca.pem

export FAB_API_ENDPOINT_FABRIC=api.fabric.microsoft.com
export FAB_SPN_CLIENT_ID=cccccccc-0000-0000-0000-000000000002
export FAB_SPN_CLIENT_SECRET=daemon-app-secret
export FAB_TENANT_ID=11111111-1111-1111-1111-111111111111
fab config set encryption_fallback_enabled true >/dev/null 2>&1   # headless: no keyring
fab config set check_cli_version_updates false >/dev/null 2>&1

echo "==> auth (service principal against entra-emulator)"
fab auth login -u "$FAB_SPN_CLIENT_ID" -p "$FAB_SPN_CLIENT_SECRET" -t "$FAB_TENANT_ID" || fail "auth"

echo "==> mkdir workspace"
fab mkdir "$WS" -P "capacityName=$CAP" || fail "create workspace"
fab exists "$WS" | grep -qi true || fail "workspace exists"

echo "==> create items (Notebook, SemanticModel, Report, DataPipeline, Lakehouse)"
for it in nb.Notebook model.SemanticModel rpt.Report pipe.DataPipeline lake.Lakehouse; do
  fab mkdir "$WS/$it" || fail "create $it"
done

echo "==> ls the workspace — every item is listed"
LS=$(fab ls "$WS") || fail "ls workspace"
echo "$LS"
for name in nb.Notebook model.SemanticModel rpt.Report pipe.DataPipeline lake.Lakehouse; do
  echo "$LS" | grep -q "$name" || fail "ls missing $name"
done

echo "==> get item properties"
fab get "$WS/nb.Notebook" -q id | grep -qE "[0-9a-f-]{36}" || fail "get item"

echo "==> fab api passthrough (raw /v1)"
fab api workspaces >/dev/null || fail "api passthrough"

echo "==> rm an item, then the workspace"
fab rm "$WS/nb.Notebook" -f || fail "rm item"
fab exists "$WS/nb.Notebook" | grep -qi false || fail "item still exists after rm"
fab rm "$WS" -f || fail "rm workspace"

echo "FABRIC-CLI E2E: PASS"
