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

# ---------------------------------------------------------------------------
# Deployment pipelines (docs/23). fab 1.6.1 has no deployment-pipeline verbs,
# so this drives them through `fab api` — still Microsoft's client, its MSAL
# auth and its HTTP stack, just without a typed wrapper.
#
# The sequence is lifted from Microsoft's own DeploymentPipelines-DeployAll.ps1
# (fabric-samples): list pipelines -> list stages -> POST deploy -> poll the
# operation -> read the result. That script is the authority on the contract,
# so matching its call order is the point of this section.
# ---------------------------------------------------------------------------
# jq is not in this image; fab emits JSON, so read fields with python3 (the
# image has it — fab is pure Python). Tolerates fab's response wrapper.
jget() { python3 -c '
import json,sys
raw=sys.stdin.read().strip()
try: d=json.loads(raw)
except Exception: sys.exit("not JSON: "+raw[:200])
for k in ("text","body","result"):          # unwrap fab api envelopes
    if isinstance(d,dict) and k in d and isinstance(d[k],(dict,list)): d=d[k]
if isinstance(d,str):
    try: d=json.loads(d)
    except Exception: pass
cur=d
for part in sys.argv[1].split("."):
    if part.isdigit(): cur=cur[int(part)]
    elif isinstance(cur,dict): cur=cur.get(part)
    else: sys.exit("no path "+sys.argv[1])
    if cur is None: sys.exit("missing "+sys.argv[1]+" in "+json.dumps(d)[:300])
print(cur)' "$1"; }

# fab decorates output (ANSI colour, CR); a GUID pasted straight into a JSON
# body then trips fab's own request parser with "invalid control character".
guid() { tr -d '\r' | sed -e 's/\x1b\[[0-9;]*[a-zA-Z]//g' | grep -oE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' | head -1; }

DPWS_SRC=dpdev.Workspace
DPWS_TGT=dptest.Workspace

echo "==> deployment pipelines: two workspaces, one item in the source"
fab mkdir "$DPWS_SRC" -P "capacityName=$CAP" || fail "create dp source workspace"
fab mkdir "$DPWS_TGT" -P "capacityName=$CAP" || fail "create dp target workspace"
fab mkdir "$DPWS_SRC/orders.Notebook" || fail "create dp item"
SRC_WS_ID=$(fab get "$DPWS_SRC" -q id | guid); [ -n "$SRC_WS_ID" ] || fail "source workspace id"
TGT_WS_ID=$(fab get "$DPWS_TGT" -q id | guid); [ -n "$TGT_WS_ID" ] || fail "target workspace id"

echo "==> create the pipeline"
fab api deploymentPipelines -X post \
  -i '{"displayName":"cli-release","description":"driven by fab"}' >/dev/null \
  || fail "create deployment pipeline"

echo "==> list pipelines, find ours by name (DeployAll step 1)"
PIPES=$(fab api deploymentPipelines) || fail "list deployment pipelines"
PID=$(echo "$PIPES" | python3 -c '
import json,sys
d=json.loads(sys.stdin.read())
for k in ("text","body","result"):
    if isinstance(d,dict) and k in d and isinstance(d[k],(dict,list)): d=d[k]
if isinstance(d,str): d=json.loads(d)
for p in d["value"]:
    if p["displayName"]=="cli-release": print(p["id"]); break
else: sys.exit("cli-release not listed")' | guid) ; [ -n "$PID" ] || fail "pipeline not found by name"

echo "==> list stages (DeployAll step 2)"
STAGES=$(fab api "deploymentPipelines/$PID/stages") || fail "list stages"
S0=$(echo "$STAGES" | jget "value.0.id" | guid); [ -n "$S0" ] || fail "stage 0 id"
S1=$(echo "$STAGES" | jget "value.1.id" | guid); [ -n "$S1" ] || fail "stage 1 id"
echo "$STAGES" | jget "value.0.displayName" | grep -q Development || fail "default stage names"

echo "==> assign workspaces to the first two stages"
ASSIGN=$(fab api "deploymentPipelines/$PID/stages/$S0/assignWorkspace" -X post \
  -i "{\"workspaceId\":\"$SRC_WS_ID\"}" 2>&1) || { echo "$ASSIGN"; fail "assign source"; }
echo "$ASSIGN"
ASSIGN2=$(fab api "deploymentPipelines/$PID/stages/$S1/assignWorkspace" -X post \
  -i "{\"workspaceId\":\"$TGT_WS_ID\"}" 2>&1) || { echo "$ASSIGN2"; fail "assign target"; }

echo "==> deploy (DeployAll step 3) — 202 + operation id"
DEPLOY=$(fab api "deploymentPipelines/$PID/deploy" -X post --show_headers \
  -i "{\"sourceStageId\":\"$S0\",\"targetStageId\":\"$S1\",\"note\":\"via fab\"}") \
  || fail "deploy"
echo "$DEPLOY"
OPID=$(echo "$DEPLOY" | python3 -c '
import json,sys,re
raw=sys.stdin.read()
m=re.search(r"[\"'\'']?x-ms-operation-id[\"'\'']?\s*[:=]\s*[\"'\'']?([0-9a-f-]{36})", raw, re.I)
print(m.group(1) if m else "")') || true
[ -n "$OPID" ] || fail "no x-ms-operation-id on the deploy 202"

echo "==> poll the operation (DeployAll step 4)"
for i in $(seq 1 30); do
  ST=$(fab api "operations/$OPID" | jget status) || fail "operation state"
  case "$ST" in
    Succeeded) break;;
    Failed) fail "deployment operation failed";;
    *) sleep 1;;
  esac
done
[ "$ST" = "Succeeded" ] || fail "operation never succeeded (last: $ST)"

echo "==> read the deployment result (DeployAll step 5)"
RESULT=$(fab api "operations/$OPID/result") || fail "operation result"
echo "$RESULT"
echo "$RESULT" | jget "items.0.displayName" | grep -q orders || fail "result missing the deployed item"
echo "$RESULT" | jget "items.0.outcome"     | grep -q Created || fail "result outcome"

echo "==> the item really landed in the target workspace"
fab exists "$DPWS_TGT/orders.Notebook" | grep -qi true || fail "item not in the target workspace"

echo "==> deploying again UPDATES the pair — no duplicate"
DEPLOY2=$(fab api "deploymentPipelines/$PID/deploy" -X post --show_headers \
  -i "{\"sourceStageId\":\"$S0\",\"targetStageId\":\"$S1\"}") || fail "second deploy"
OPID2=$(echo "$DEPLOY2" | python3 -c '
import sys,re
m=re.search(r"[\"'\'']?x-ms-operation-id[\"'\'']?\s*[:=]\s*[\"'\'']?([0-9a-f-]{36})", sys.stdin.read(), re.I)
print(m.group(1) if m else "")')
[ -n "$OPID2" ] || fail "no operation id on the second deploy"
for i in $(seq 1 30); do
  ST2=$(fab api "operations/$OPID2" | jget status); [ "$ST2" = "Succeeded" ] && break; sleep 1
done
fab api "operations/$OPID2/result" | jget "items.0.outcome" | grep -q Updated \
  || fail "second deploy did not UPDATE the paired item"
COUNT=$(fab ls "$DPWS_TGT" | grep -c "orders.Notebook" || true)
[ "$COUNT" = "1" ] || fail "second deploy duplicated the item (count=$COUNT)"

echo "==> deployment history records both deploys, newest first"
# note is omitempty and the second deploy sent none, so assert on the shape:
# two entries, newest first, and the older one carries the note we sent.
fab api "deploymentPipelines/$PID/operations" | python3 -c '
import json,sys
d=json.loads(sys.stdin.read())
for k in ("text","body","result"):
    if isinstance(d,dict) and k in d and isinstance(d[k],(dict,list)): d=d[k]
if isinstance(d,str): d=json.loads(d)
ops=d["value"]
assert len(ops)==2, f"want 2 deployments, got {len(ops)}: {ops}"
assert ops[1].get("note")=="via fab", f"oldest deployment lost its note: {ops[1]}"
assert ops[0]["items"][0]["outcome"]=="Updated", f"newest is not the re-deploy: {ops[0]}"
assert ops[1]["items"][0]["outcome"]=="Created", f"oldest is not the first deploy: {ops[1]}"
print("history ok")' || fail "deployment operations history"

echo "==> tidy up the deployment-pipeline fixtures"
fab api "deploymentPipelines/$PID" -X delete >/dev/null || fail "delete pipeline"
fab rm "$DPWS_SRC" -f >/dev/null || fail "rm dp source workspace"
fab rm "$DPWS_TGT" -f >/dev/null || fail "rm dp target workspace"

# ---------------------------------------------------------------------------
# Pagination (docs/parity.md, list continuationToken). Until now the token
# contract had no real-client witness: we wrote both the producer and the
# consumer, so our Go tests agreed with themselves by construction. Here
# Microsoft's client transmits the opaque token through its own HTTP stack and
# query handling, and the pages are checked for completeness — no item seen
# twice, none missed, and the terminal page carrying no token.
# ---------------------------------------------------------------------------
echo "==> pagination: page the workspace items with a real client"
WSID=$(fab get "$WS" -q id | guid); [ -n "$WSID" ] || fail "workspace id for pagination"

# 5 items already exist above; a page size of 2 forces three pages.
PAGES=0; SEEN=""; TOK=""
for i in $(seq 1 10); do
  if [ -z "$TOK" ]; then
    BODY=$(fab api "workspaces/$WSID/items?maxPageSize=2") || fail "list page $i"
  else
    BODY=$(fab api "workspaces/$WSID/items?maxPageSize=2&continuationToken=$TOK") || fail "list page $i"
  fi
  PAGES=$((PAGES+1))
  IDS=$(echo "$BODY" | python3 -c '
import json,sys
d=json.loads(sys.stdin.read())
for k in ("text","body","result"):
    if isinstance(d,dict) and k in d and isinstance(d[k],(dict,list)): d=d[k]
if isinstance(d,str): d=json.loads(d)
print(" ".join(i["id"] for i in d["value"]))') || fail "page $i body"
  SEEN="$SEEN $IDS"
  TOK=$(echo "$BODY" | python3 -c '
import json,sys
d=json.loads(sys.stdin.read())
for k in ("text","body","result"):
    if isinstance(d,dict) and k in d and isinstance(d[k],(dict,list)): d=d[k]
if isinstance(d,str): d=json.loads(d)
print(d.get("continuationToken",""))' | tr -d "\r")
  [ -z "$TOK" ] && break
done
[ -n "$TOK" ] && fail "pagination never terminated (token still set after $PAGES pages)"
[ "$PAGES" -ge 3 ] || fail "expected at least 3 pages at maxPageSize=2, got $PAGES"

# Every item exactly once, and the paged set equals the unpaged set.
ALL=$(fab api "workspaces/$WSID/items" | python3 -c '
import json,sys
d=json.loads(sys.stdin.read())
for k in ("text","body","result"):
    if isinstance(d,dict) and k in d and isinstance(d[k],(dict,list)): d=d[k]
if isinstance(d,str): d=json.loads(d)
print(" ".join(sorted(i["id"] for i in d["value"])))')
PAGED=$(echo "$SEEN" | tr " " "\n" | grep -v "^$" | sort | tr "\n" " " | sed "s/ $//")
UNIQ=$(echo "$SEEN" | tr " " "\n" | grep -v "^$" | sort -u | tr "\n" " " | sed "s/ $//")
[ "$PAGED" = "$UNIQ" ] || fail "an item was returned on more than one page"
[ "$PAGED" = "$ALL" ]  || fail "paged set != unpaged set (paged=[$PAGED] all=[$ALL])"
echo "    paged $PAGES pages, $(echo $UNIQ | wc -w | tr -d ' ') distinct items, no duplicates or gaps"

echo "==> continuationUri is an absolute URL, as real Fabric returns"
fab api "workspaces/$WSID/items?maxPageSize=2" | python3 -c '
import json,sys,urllib.parse
d=json.loads(sys.stdin.read())
for k in ("text","body","result"):
    if isinstance(d,dict) and k in d and isinstance(d[k],(dict,list)): d=d[k]
if isinstance(d,str): d=json.loads(d)
uri=d.get("continuationUri")
assert uri, "no continuationUri on a page that has a continuationToken"
u=urllib.parse.urlparse(uri)
assert u.scheme and u.netloc, f"continuationUri is not absolute: {uri}"
assert urllib.parse.parse_qs(u.query).get("continuationToken") == [d["continuationToken"]], \
    f"continuationUri carries a different token: {uri}"
print(f"    continuationUri ok: {u.scheme}://{u.netloc}{u.path}")' || fail "continuationUri shape"

echo "==> rm an item, then the workspace"
fab rm "$WS/nb.Notebook" -f || fail "rm item"
fab exists "$WS/nb.Notebook" | grep -qi false || fail "item still exists after rm"
fab rm "$WS" -f || fail "rm workspace"

echo "FABRIC-CLI E2E: PASS"
