# Start the DAX pump against Desktop's msmdsrv and run smoke.py.
#
# Reads the port file desktop.ps1 named (same PORTFILE line e2e/pbix-desktop/run.py
# parses). Leaves the pump in the foreground's sibling process so /health can
# come up while Python waits.
param(
  [Parameter(Mandatory = $true)][string]$DesktopLog,
  [string]$Listen = "http://127.0.0.1:8080"
)

$ErrorActionPreference = "Stop"
$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$repo = Resolve-Path (Join-Path $here "..\..")

$log = Get-Content -LiteralPath $DesktopLog -Raw
if ($log -notmatch '(?m)^PORTFILE (.+)$') {
  Write-Error "desktop log named no PORTFILE"
}
$portFile = $Matches[1].Trim()
$port = (Get-Content -LiteralPath $portFile -Encoding Unicode).Trim()
if (-not $port) { Write-Error "empty port file $portFile" }
Write-Host "MSMDSRV_PORT=$port  ($portFile)"

$env:MSMDSRV_PORT = $port
$env:MSMDSRV_PUMP_ADDR = $Listen
# Do not set MSMDSRV_CATALOG — Desktop already has the open .pbix catalog.

dotnet build (Join-Path $here "pump") -c Release --nologo
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
$exe = Join-Path $here "pump\bin\Release\net8.0\daxpump.exe"
$proc = Start-Process -FilePath $exe -PassThru -NoNewWindow
if (-not $proc) { Write-Error "failed to start daxpump" }
try {
  & uv run --frozen --no-sync python (Join-Path $here "smoke.py") --pump $Listen
  exit $LASTEXITCODE
} finally {
  if (-not $proc.HasExited) { Stop-Process -Id $proc.Id -Force }
}
