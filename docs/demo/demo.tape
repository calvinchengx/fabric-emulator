# Regenerate the README demo GIF:  vhs docs/demo/demo.tape   (run from the repo root)
# Requires: vhs, ttyd, ffmpeg, go, curl, jq, entra-emulator on PATH.
#
# NOTE ON QUOTING: VHS's lexer has no escape sequence, so `Type "... \" ..."`
# fails to parse. Commands that need double quotes are delimited with backticks
# instead — inside those, both ' and " pass through literally.
Output docs/demo/demo.gif

Require go
Require curl
Require jq
Require entra-emulator

Set Shell bash
Set FontSize 19
Set Width 1240
Set Height 680
Set Padding 22
Set Margin 16
Set MarginFill "#1e1e2e"
Set BorderRadius 10
Set WindowBar Colorful
Set Theme "Catppuccin Mocha"
# 10ms, not the 18ms the entra demo uses: these commands are long (bearer
# headers, api-version query strings), so the typing animation — not the
# pauses — dominates the runtime. Slower typing here just makes a reader wait.
Set TypingSpeed 10ms

# --- setup (not shown) ---
# Two binaries, no containers: the emulator validates real Entra tokens, so the
# issuer has to be up first. Ports are deliberately odd so a running dev stack
# on :8443/:9443 does not collide with the recording.
Hide
Type "go build -o /tmp/fe-demo ./cmd/fabric-emulator" Enter
Sleep 8s
# mkdir, not just rm: the emulator opens its SQLite file inside -data-dir and
# will not create the directory, so a bare rm -rf leaves it failing to start
# with "unable to open database file (14)".
Type "rm -rf /tmp/fe-demo-data /tmp/ee-demo.db && mkdir -p /tmp/fe-demo-data" Enter
Type "TLS_ENABLED=false PORT=18099 ORIGIN_MODE=compat DB_PATH=/tmp/ee-demo.db SEED_ON_START=true entra-emulator >/tmp/ee-demo.log 2>&1 &" Enter
Type "until curl -sf localhost:18099/health >/dev/null 2>&1; do sleep 0.3; done" Enter
Type "/tmp/fe-demo -addr 127.0.0.1:19099 -disable-tls -data-dir /tmp/fe-demo-data -entra-issuer http://localhost:18099/11111111-1111-1111-1111-111111111111/v2.0 >/tmp/fe-demo.log 2>&1 &" Enter
Type "until curl -sf localhost:19099/health >/dev/null 2>&1; do sleep 0.3; done" Enter
Sleep 2s
Type "PS1='$ '" Enter
Type "F=http://localhost:19099" Enter
Type "T=http://localhost:18099/11111111-1111-1111-1111-111111111111/oauth2/v2.0/token" Enter
Type "C=cccccccc-0000-0000-0000-000000000002" Enter
Type "SEC=daemon-app-secret" Enter
Type "clear" Enter
Show

Sleep 500ms
Type "# fabric-emulator - Microsoft Fabric, local, one Go binary" Enter
Sleep 600ms

Type "# 1. A real Entra token - the client-credentials flow Fabric requires" Enter
Sleep 500ms
Type "TOK=$(curl -s -X POST $T -d grant_type=client_credentials -d client_id=$C -d client_secret=$SEC -d scope=https://api.fabric.microsoft.com/.default | jq -r .access_token)" Enter
Sleep 800ms
Type "echo ${TOK:0:40}..." Enter
Sleep 800ms
Type `A="Authorization: Bearer $TOK"` Enter
Sleep 400ms

Type "# 2. Create a workspace - real REST, real RBAC" Enter
Sleep 500ms
Type `WS=$(curl -s -X POST $F/v1/workspaces -H "$A" -d '{"displayName":"demo"}' | jq -r .id)` Enter
Sleep 700ms
Type `curl -s $F/v1/workspaces/$WS -H "$A" | jq '{id, displayName, type}'` Enter
Sleep 1300ms

Type "# 3. A lakehouse inside it - a typed Fabric item" Enter
Sleep 500ms
Type `curl -s -X POST $F/v1/workspaces/$WS/lakehouses -H "$A" -d '{"displayName":"lake"}' | jq '{displayName, type}'` Enter
Sleep 1400ms

Type "# 4. Write a file into OneLake - the ADLS Gen2 surface, unchanged" Enter
Sleep 500ms
Type "K=$(curl -s -X POST $T -d grant_type=client_credentials -d client_id=$C -d client_secret=$SEC -d scope=https://storage.azure.com/.default | jq -r .access_token)" Enter
Sleep 700ms
Type `B="Authorization: Bearer $K"` Enter
Sleep 400ms
Type `S=$F/onelake/demo/lake.Lakehouse/Files/hello.txt` Enter
Sleep 400ms
Type `curl -s -X PUT $S -H "$B" -H 'x-ms-blob-type: BlockBlob' --data 'real bytes on disk' -o /dev/null -w 'PUT -> %{http_code}\n'` Enter
Sleep 1000ms
Type `curl -s $S -H "$B"; echo` Enter
Sleep 1100ms

Type "" Enter
Type "# No tenant. No capacity. Real JWTs, real RBAC, real bytes on disk." Enter
Sleep 1300ms

# --- teardown (not shown) ---
Hide
Type "kill %1 %2 2>/dev/null" Enter
Show
