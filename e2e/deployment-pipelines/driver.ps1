# Microsoft's deployment-pipeline automation, run against the emulator with
# Azure PowerShell — the same Connect-AzAccount / Get-AzAccessToken flow as
# fabric-samples' DeploymentPipelines-DeployAll.ps1, then that script's exact
# REST sequence.
#
# This is a second, independent client family for the deployment-pipeline
# surface: the fabric-cli e2e drives it from Python/MSAL, this one from
# .NET/MSAL via Az. Staged deliberately — each step prints before it runs, so
# a failure names the stage instead of surfacing as a bare hang.

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'

$tenant   = '11111111-1111-1111-1111-111111111111'
$clientId = 'cccccccc-0000-0000-0000-000000000002'
$secret   = 'daemon-app-secret'
$authority = 'https://login.microsoftonline.com/'   # NO port: MSAL drops one
$fabric    = 'https://api.fabric.microsoft.com/v1'  # DeployAll's $global:baseUrl

function Step($m) { Write-Host "==> $m" }
function Fail($m) { Write-Host "DEPLOYMENT-PIPELINES PS E2E: FAIL ($m)"; exit 1 }

Step 'waiting for entra + fabric TLS'
foreach ($h in 'login.microsoftonline.com', 'api.fabric.microsoft.com') {
    $ok = $false
    foreach ($i in 1..90) {
        # PowerShell reserves '<', so stdin is closed by piping an empty string.
        $r = '' | & openssl s_client -connect "${h}:443" -servername $h 2>$null
        if ($r -match 'BEGIN CERTIFICATE') { $ok = $true; break }
        Start-Sleep -Seconds 1
    }
    if (-not $ok) { Fail "$h never came up" }
}

# .NET on Linux validates against the OpenSSL store, and MSAL offers no
# certificate-validation bypass (Connect-AzAccount's -SkipValidation is about
# environment metadata, not TLS). So the emulator's self-signed roots go into
# the system store for real.
Step 'trusting the emulator certificates'
foreach ($h in 'login.microsoftonline.com', 'api.fabric.microsoft.com') {
    $pem = '' | & openssl s_client -connect "${h}:443" -servername $h 2>$null |
           & openssl x509
    $pem | Set-Content -Path "/usr/local/share/ca-certificates/$h.crt"
}
& update-ca-certificates 2>&1 | Out-Null

Step 'verifying .NET trusts them (fails fast rather than hanging in MSAL)'
try {
    $c = [System.Net.Http.HttpClient]::new()
    $c.Timeout = [TimeSpan]::FromSeconds(15)
    $null = $c.GetStringAsync("$authority" + 'health').GetAwaiter().GetResult()
} catch {
    Fail "dotnet does not trust the entra certificate: $($_.Exception.GetBaseException().Message)"
}

Step 'Add-AzEnvironment (custom cloud pointing at the emulator)'
Import-Module Az.Accounts
# ResourceManagerUrl is mandatory (omitting it throws "Value cannot be null.
# (Parameter 'uriString')"), but there is no ARM in this family. It must NOT
# point at an origin root: entra-emulator's portal SPA answers any unknown
# ROOT-level GET with 200 text/html, so Az's ARM metadata probe
# (/metadata/endpoints) receives a web page and dies with "Unexpected
# character encountered while parsing value: <". Tenant-prefixed paths escape
# the SPA fallback and 404 as JSON, which Az tolerates — so point it at one.
# See docs/23 "Running Microsoft's PowerShell sample".
Add-AzEnvironment -Name FabricEmulator `
    -ActiveDirectoryAuthority $authority `
    -ActiveDirectoryServiceEndpointResourceId 'https://api.fabric.microsoft.com' `
    -ResourceManagerUrl "$authority$tenant/no-arm/" | Out-Null

Step 'Connect-AzAccount -ServicePrincipal (MSAL against entra-emulator)'
$cred = New-Object System.Management.Automation.PSCredential(
    $clientId, (ConvertTo-SecureString $secret -AsPlainText -Force))
try {
    # -SkipContextPopulation: no ARM behind this cloud, so subscription
    # enumeration has nothing to enumerate.
    Connect-AzAccount -Environment FabricEmulator -ServicePrincipal `
        -TenantId $tenant -Credential $cred -SkipContextPopulation | Out-Null
} catch {
    Fail "Connect-AzAccount: $($_.Exception.GetBaseException().Message)"
}

Step 'Get-AzAccessToken for the Fabric audience'
$tok = Get-AzAccessToken -ResourceUrl 'https://api.fabric.microsoft.com' -AsSecureString
$bearer = [System.Net.NetworkCredential]::new('', $tok.Token).Password
if (-not $bearer) { Fail 'no token' }
Write-Host "    token acquired (len=$($bearer.Length))"
$headers = @{ Authorization = "Bearer $bearer"; 'Content-Type' = 'application/json' }

# ---- DeployAll.ps1's REST sequence, in its order -------------------------
Step 'seed: two workspaces and an item in the source'
$srcWs = Invoke-RestMethod -Method Post -Uri "$fabric/workspaces" -Headers $headers `
    -Body (@{ displayName = 'ps-dev' } | ConvertTo-Json)
$tgtWs = Invoke-RestMethod -Method Post -Uri "$fabric/workspaces" -Headers $headers `
    -Body (@{ displayName = 'ps-test' } | ConvertTo-Json)
Invoke-RestMethod -Method Post -Uri "$fabric/workspaces/$($srcWs.id)/items" -Headers $headers `
    -Body (@{ displayName = 'orders'; type = 'Notebook' } | ConvertTo-Json) | Out-Null

Step 'create the deployment pipeline'
Invoke-RestMethod -Method Post -Uri "$fabric/deploymentPipelines" -Headers $headers `
    -Body (@{ displayName = 'ps-release'; description = 'driven by Az PowerShell' } | ConvertTo-Json) | Out-Null

Step 'GET /deploymentPipelines — find it by name (DeployAll step 1)'
$pipelines = Invoke-RestMethod -Method Get -Uri "$fabric/deploymentPipelines" -Headers $headers
$pipeline = $pipelines.value | Where-Object { $_.displayName -eq 'ps-release' }
if (-not $pipeline) { Fail 'pipeline not listed' }

Step 'GET /deploymentPipelines/{id}/stages (DeployAll step 2)'
$stages = (Invoke-RestMethod -Method Get -Headers $headers `
    -Uri "$fabric/deploymentPipelines/$($pipeline.id)/stages").value
if ($stages.Count -ne 3) { Fail "want 3 default stages, got $($stages.Count)" }
if ($stages[0].displayName -ne 'Development') { Fail 'default stage names' }

Step 'assign workspaces to the first two stages'
Invoke-RestMethod -Method Post -Headers $headers `
    -Uri "$fabric/deploymentPipelines/$($pipeline.id)/stages/$($stages[0].id)/assignWorkspace" `
    -Body (@{ workspaceId = $srcWs.id } | ConvertTo-Json) | Out-Null
Invoke-RestMethod -Method Post -Headers $headers `
    -Uri "$fabric/deploymentPipelines/$($pipeline.id)/stages/$($stages[1].id)/assignWorkspace" `
    -Body (@{ workspaceId = $tgtWs.id } | ConvertTo-Json) | Out-Null

Step 'POST /deploy (DeployAll step 3) — 202 + operation id'
$resp = Invoke-WebRequest -Method Post -Headers $headers `
    -Uri "$fabric/deploymentPipelines/$($pipeline.id)/deploy" `
    -Body (@{ sourceStageId = $stages[0].id; targetStageId = $stages[1].id
              note = 'via Az PowerShell' } | ConvertTo-Json)
if ($resp.StatusCode -ne 202) { Fail "deploy returned $($resp.StatusCode), want 202" }
$opId = $resp.Headers['x-ms-operation-id']
if ($opId -is [array]) { $opId = $opId[0] }
if (-not $opId) { Fail 'no x-ms-operation-id on the 202' }

Step 'poll the operation, honouring Retry-After (DeployAll step 4)'
$retry = $resp.Headers['Retry-After']; if ($retry -is [array]) { $retry = $retry[0] }
if (-not $retry) { $retry = 1 }
$state = $null
foreach ($i in 1..30) {
    $state = Invoke-RestMethod -Method Get -Uri "$fabric/operations/$opId" -Headers $headers
    if ($state.status -in @('Succeeded', 'Failed')) { break }
    Start-Sleep -Seconds ([int]$retry)
}
if ($state.status -ne 'Succeeded') { Fail "operation status $($state.status)" }

Step 'GET /operations/{id}/result — the extended detail (DeployAll step 5)'
$result = Invoke-RestMethod -Method Get -Uri "$fabric/operations/$opId/result" -Headers $headers
if ($result.items.Count -ne 1) { Fail "want 1 deployed item, got $($result.items.Count)" }
if ($result.items[0].displayName -ne 'orders') { Fail 'wrong item deployed' }
if ($result.items[0].outcome -ne 'Created') { Fail "first deploy outcome $($result.items[0].outcome)" }

Step 'the item really landed in the target workspace, with a new id'
$tgtItems = (Invoke-RestMethod -Method Get -Headers $headers `
    -Uri "$fabric/workspaces/$($tgtWs.id)/items").value
if ($tgtItems.Count -ne 1 -or $tgtItems[0].displayName -ne 'orders') {
    Fail "target items: $($tgtItems | ConvertTo-Json -Compress)"
}
if ($tgtItems[0].id -ne $result.items[0].targetItemId) { Fail 'result id disagrees with the workspace' }

Step 'a second deploy UPDATES the pair — no duplicate'
$resp2 = Invoke-WebRequest -Method Post -Headers $headers `
    -Uri "$fabric/deploymentPipelines/$($pipeline.id)/deploy" `
    -Body (@{ sourceStageId = $stages[0].id; targetStageId = $stages[1].id } | ConvertTo-Json)
$opId2 = $resp2.Headers['x-ms-operation-id']; if ($opId2 -is [array]) { $opId2 = $opId2[0] }
foreach ($i in 1..30) {
    $s2 = Invoke-RestMethod -Method Get -Uri "$fabric/operations/$opId2" -Headers $headers
    if ($s2.status -in @('Succeeded', 'Failed')) { break }
    Start-Sleep -Seconds 1
}
$result2 = Invoke-RestMethod -Method Get -Uri "$fabric/operations/$opId2/result" -Headers $headers
if ($result2.items[0].outcome -ne 'Updated') { Fail "second deploy outcome $($result2.items[0].outcome)" }
$after = (Invoke-RestMethod -Method Get -Headers $headers `
    -Uri "$fabric/workspaces/$($tgtWs.id)/items").value
if ($after.Count -ne 1) { Fail "second deploy duplicated the item (count=$($after.Count))" }

Write-Host 'DEPLOYMENT-PIPELINES PS E2E: PASS'
