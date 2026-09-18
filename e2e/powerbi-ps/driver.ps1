# MicrosoftPowerBIMgmt -- Microsoft's own PowerShell module for the Power BI
# REST surface -- driven against the emulator exactly as it would drive a
# tenant. Nothing here reconfigures the module: its stock `Public` environment
# resolves to https://api.powerbi.com and https://login.microsoftonline.com,
# and the compose file makes the emulator answer at both.
#
# WHY THIS SUITE EXISTS. The admin and tenant surfaces below were witnessed
# only by `ci:az-rest`, and `az rest` is a TRANSPORT rather than a client: it
# sends the request the script wrote and hands back the body, carrying no model
# of Power BI at all. A shape this emulator returned wrongly would be caught
# only if the script happened to assert on the wrong field.
#
# These cmdlets are different in kind. `Get-PowerBIWorkspace` and its siblings
# DESERIALISE INTO .NET TYPES the module ships, so a missing or misnamed field
# fails inside Microsoft's code before any assertion here runs. That is the
# independence the witness is credited for.
#
# `Invoke-PowerBIRestMethod` is deliberately used only where no typed cmdlet
# exists, and the manifest does not credit those calls as typed -- see the
# note in docs/witnesses.json. It is az-rest with a different auth stack.
#
# Staged deliberately: each step prints before it runs, so a failure names the
# stage instead of surfacing as a bare hang.

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'

$tenant   = '6f89cf12-978b-4d23-ac18-9ef0c127cf87'
$clientId = '00d88624-f0d7-46f6-a641-6232c2608928'
$secret   = 'daemon-app-secret'
$hosts    = 'login.microsoftonline.com', 'api.fabric.microsoft.com', 'api.powerbi.com'

function Step($m) { Write-Host "==> $m" }
function Fail($m) { Write-Host "POWERBI-PS E2E: FAIL ($m)"; exit 1 }

Step 'waiting for entra + fabric TLS'
foreach ($h in $hosts) {
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
# certificate-validation bypass. So the emulator's self-signed roots go into
# the system store for real.
Step 'trusting the emulator certificates'
foreach ($h in $hosts) {
    $pem = '' | & openssl s_client -connect "${h}:443" -servername $h 2>$null |
           & openssl x509
    $pem | Set-Content -Path "/usr/local/share/ca-certificates/$h.crt"
}
& update-ca-certificates 2>&1 | Out-Null

Step 'verifying .NET trusts them (fails fast rather than hanging in MSAL)'
try {
    $c = [System.Net.Http.HttpClient]::new()
    $c.Timeout = [TimeSpan]::FromSeconds(15)
    $null = $c.GetStringAsync('https://login.microsoftonline.com/health').GetAwaiter().GetResult()
} catch {
    Fail "dotnet does not trust the entra certificate: $($_.Exception.GetBaseException().Message)"
}

Step 'Import-Module MicrosoftPowerBIMgmt'
Import-Module MicrosoftPowerBIMgmt
$moduleVersion = (Get-Module MicrosoftPowerBIMgmt).Version.ToString()
Write-Host "    module $moduleVersion"

Step 'Connect-PowerBIServiceAccount (service principal, stock Public cloud)'
$cred = [PSCredential]::new($clientId, (ConvertTo-SecureString $secret -AsPlainText -Force))
try {
    $null = Connect-PowerBIServiceAccount -ServicePrincipal -Credential $cred -TenantId $tenant
} catch {
    Fail "Connect-PowerBIServiceAccount: $($_.Exception.GetBaseException().Message)"
}

# --- typed cmdlets: the claims this suite is credited for ---------------------
#
# THE WORKSPACE IS CREATED BY A DIFFERENT CLIENT ON A DIFFERENT TOKEN, and that
# is the point rather than a workaround. `New-PowerBIWorkspace` is NOT served:
# POST /v1.0/myorg/groups returns 404, recorded as a gap in docs/parity.md
# rather than filled to make this suite tidier.
#
# Writing through the Fabric API and reading back through the Power BI module
# is the stronger shape in any case. A suite that writes and reads with one
# client can agree with itself about a wrong answer; this one cannot, because
# the writer never sees what the reader deserialises. The two tokens are also
# different audiences -- api.fabric.microsoft.com here, the legacy
# analysis.windows.net/powerbi/api resource in the module.

$name = "pbips-$([guid]::NewGuid().ToString('N').Substring(0,8))"

Step 'client-credentials token for the Fabric audience (not via the module)'
$tokenUri = "https://login.microsoftonline.com/$tenant/oauth2/v2.0/token"
$body = @{
    grant_type    = 'client_credentials'
    client_id     = $clientId
    client_secret = $secret
    scope         = 'https://api.fabric.microsoft.com/.default'
}
try {
    $tok = (Invoke-RestMethod -Method POST -Uri $tokenUri -Body $body).access_token
} catch {
    Fail "client-credentials token: $($_.Exception.GetBaseException().Message)"
}
if (-not $tok) { Fail 'no access_token in the token response' }

Step "POST /v1/workspaces $name (Fabric API, the writer)"
try {
    $created = Invoke-RestMethod -Method POST -Uri 'https://api.fabric.microsoft.com/v1/workspaces' `
        -Headers @{ Authorization = "Bearer $tok" } `
        -ContentType 'application/json' -Body (@{ displayName = $name } | ConvertTo-Json)
} catch {
    Fail "create workspace: $($_.Exception.GetBaseException().Message)"
}
if (-not $created.id) { Fail 'created workspace carried no id' }
Write-Host "    id $($created.id)"

Step 'Get-PowerBIWorkspace (the reader: a typed round-trip of what Fabric wrote)'
# The assertion that matters is not the -eq below: it is that the module
# DESERIALISED the response into its own Workspace type without throwing. A
# renamed or missing field fails inside Microsoft's code, above this line, and
# that is the independence `az rest` cannot provide.
$mine = Get-PowerBIWorkspace | Where-Object { $_.Id -eq $created.id }
if (-not $mine) { Fail "workspace $($created.id) absent from Get-PowerBIWorkspace" }
if ($mine.Name -ne $name) { Fail "name round-tripped as '$($mine.Name)', expected '$name'" }

Step 'Get-PowerBIWorkspace -Scope Organization is NOT served, and that is pinned'
# The module calls the POWER BI spelling, GET /v1.0/myorg/admin/groups. The
# emulator serves the FABRIC spelling, GET /v1/admin/workspaces, which is what
# the `Tenant-wide workspace admin` claim is about -- so the claim is true and
# this is a different, unimplemented API rather than a broken one.
#
# HOW IT FAILS WAS ITSELF A FINDING, and it has since been fixed. The
# unrouted path fell through to the portal and answered HTML, so a typed
# client reported "Unable to deserialize the response" -- which reads like a
# serialisation bug in the surface the caller asked for rather than a route
# that does not exist. The Power BI roots are now in apiPrefixes and this
# answers Power BI's own 404 envelope, so the refusal below names the gap.
$refusedOrg = $false
try { $null = Get-PowerBIWorkspace -Scope Organization } catch { $refusedOrg = $true }
if (-not $refusedOrg) { Fail 'Get-PowerBIWorkspace -Scope Organization unexpectedly worked -- regrade the parity row' }

Step 'Get-PowerBIDataset -WorkspaceId (typed; empty is the correct answer here)'
# An empty collection is correct for a workspace with no semantic model. What
# is witnessed is that the ENVELOPE deserialises, not that anything is in it --
# stated plainly so the claim is not read as more than it is.
$datasets = @(Get-PowerBIDataset -WorkspaceId $created.id)
Write-Host "    $($datasets.Count) dataset(s)"

Step 'Get-PowerBIActivityEvent (admin activity log, one UTC day)'
# The single-UTC-day window is the service's own constraint, enforced by the
# module before the request leaves -- so this also witnesses that the emulator
# accepts the exact DateTime spelling a real client sends, quotes included.
$day   = (Get-Date).ToUniversalTime().Date
$start = $day.ToString('yyyy-MM-ddTHH:mm:ss')
$end   = $day.AddSeconds(86399).ToString('yyyy-MM-ddTHH:mm:ss')
try {
    $events = Get-PowerBIActivityEvent -StartDateTime $start -EndDateTime $end
} catch {
    Fail "Get-PowerBIActivityEvent: $($_.Exception.GetBaseException().Message)"
}
if ($null -eq $events) { Fail 'Get-PowerBIActivityEvent returned nothing at all' }
Write-Host "    activity payload length $($events.Length)"

# THE UNSERVED CMDLETS, PINNED AS A GROUP RATHER THAN SKIPPED.
#
# Each of these calls a documented Power BI route the emulator does not
# implement, listed with its path in docs/parity.md. They are asserted to FAIL
# so the gap cannot quietly close or widen: when one is implemented this loop
# fails, which is the reminder to regrade the row rather than leave the map
# claiming less than the code does.
#
# HOW THEY FAIL WAS THE FINDING, and it is fixed. An unrouted /v1.0/myorg path
# fell through to the portal and answered HTML, so a typed client reported
# "Unable to deserialize the response" rather than the plain 404 that names the
# gap. `az rest` prints the HTML and moves on, which is why nothing caught it
# until a typed client was pointed at the surface. These now fail with Power
# BI's own 404 envelope -- still refusals, but legible ones.
Step 'the unserved Power BI cmdlets are asserted to fail, not skipped'
$unserved = @(
    @{ n = 'Get-PowerBIReport -WorkspaceId'; s = { Get-PowerBIReport -WorkspaceId $created.id } },
    @{ n = 'Get-PowerBIReport';              s = { Get-PowerBIReport } },
    @{ n = 'Get-PowerBICapacity';            s = { Get-PowerBICapacity } }
)
foreach ($u in $unserved) {
    $failed = $false
    try { $null = & $u.s } catch { $failed = $true }
    if (-not $failed) { Fail "$($u.n) unexpectedly worked -- regrade the parity row" }
    Write-Host "    refused as expected: $($u.n)"
}

Step 'New-PowerBIWorkspace is NOT served, and the refusal is asserted'
# Pinned deliberately. POST /v1.0/myorg/groups is not implemented, docs/parity.md
# says so, and a test that merely skipped it would let the gap close or widen
# unnoticed. When it IS implemented this assertion fails, which is the reminder
# to regrade the row.
$refused = $false
try { $null = New-PowerBIWorkspace -Name "$name-2" } catch { $refused = $true }
if (-not $refused) { Fail 'New-PowerBIWorkspace unexpectedly succeeded -- regrade the parity row' }

Write-Host "POWERBI-PS E2E: PASS (MicrosoftPowerBIMgmt $moduleVersion)"
