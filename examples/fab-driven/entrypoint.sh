#!/usr/bin/env bash
# Make `fab` trust the emulator, log it in, then get out of the way.
#
# Everything this script prints goes to STDERR. `fab`'s own stdout is left
# untouched, so a caller running `fab ... --output_format json` can parse the
# result without first filtering our chatter out of it — see fabctl.py, which
# does exactly that.
set -euo pipefail

TENANT="${FAB_TENANT_ID:-6f89cf12-978b-4d23-ac18-9ef0c127cf87}"
AUTHORITY_HOST=login.microsoftonline.com
API_HOST=api.fabric.microsoft.com
# The OneLake DATA plane is a separate hostname, and forgetting it is not a
# theoretical risk: `fab cp` failed here with CERTIFICATE_VERIFY_FAILED against
# a host no alias and no CA bundle had ever heard of, while every control-plane
# command kept working.
ONELAKE_HOST=onelake.dfs.fabric.microsoft.com

exec 3>&1   # keep the real stdout
exec 1>&2   # bootstrap noise goes to stderr

# The certificates. `fab` uses requests, which reads REQUESTS_CA_BUNDLE, and
# MSAL, which reads SSL_CERT_FILE — both are needed, and they are different
# variables in different libraries, which is why both are set below.
wait_tls() {
  local host=$1
  for _ in $(seq 1 90); do
    if openssl s_client -connect "${host}:443" -servername "$host" </dev/null 2>/dev/null \
        | grep -q "BEGIN CERTIFICATE"; then
      return 0
    fi
    sleep 1
  done
  echo "fab: ${host}:443 never presented a certificate — is the stack up?" >&2
  return 1
}

: > /tmp/ca.pem
for host in "$AUTHORITY_HOST" "$API_HOST" "$ONELAKE_HOST"; do
  wait_tls "$host"
  openssl s_client -connect "${host}:443" -servername "$host" </dev/null 2>/dev/null \
    | openssl x509 >> /tmp/ca.pem
done
export REQUESTS_CA_BUNDLE=/tmp/ca.pem SSL_CERT_FILE=/tmp/ca.pem

export FAB_API_ENDPOINT_FABRIC="$API_HOST"
export FAB_API_ENDPOINT_ONELAKE="$ONELAKE_HOST"
export FAB_SPN_CLIENT_ID="${FAB_SPN_CLIENT_ID:-00d88624-f0d7-46f6-a641-6232c2608928}"
export FAB_SPN_CLIENT_SECRET="${FAB_SPN_CLIENT_SECRET:-daemon-app-secret}"
export FAB_TENANT_ID="$TENANT"

# No keyring in a container, so fall back to fab's file-backed encryption; and
# never let a version check reach the internet mid-run.
fab config set encryption_fallback_enabled true  >/dev/null 2>&1 || true
fab config set check_cli_version_updates false   >/dev/null 2>&1 || true

# A service principal, through Microsoft's own MSAL, against the emulator's
# token endpoint. Nothing about this handshake is faked on the client side.
fab auth login -u "$FAB_SPN_CLIENT_ID" -p "$FAB_SPN_CLIENT_SECRET" -t "$FAB_TENANT_ID" >/dev/null

exec 1>&3   # restore stdout for fab itself
exec fab "$@"
