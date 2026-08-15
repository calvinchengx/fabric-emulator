# Run the DAX pump inside a Windows guest (UTM / dockur / native).
#
# Discovers Desktop's msmdsrv port the same way e2e/pbix-desktop/desktop.ps1
# does, then listens on 0.0.0.0:8080 so the Mac/Linux emulator can reach it.
# Set MSMDSRV_PORT yourself to skip discovery (SSAS, or a known Desktop port).
# The emulator POSTs /v1/deploy (TMSL) then /v1/dax; Desktop that will not
# create a database returns 409 and the query uses the open catalog.
#
#   pwsh e2e/msmdsrv/start.ps1
#   # then on the emulator host:
#   export FABRIC_DAX_URL=http://<guest-ip>:8080
#
# See docs/52-msmdsrv-hosts.md.
param(
  [string]$Listen = $(if ($env:MSMDSRV_PUMP_ADDR) { $env:MSMDSRV_PUMP_ADDR } else { "http://0.0.0.0:8080" })
)

$ErrorActionPreference = "Stop"
$here = Split-Path -Parent $MyInvocation.MyCommand.Path

if (-not $env:MSMDSRV_PORT -and -not $env:MSMDSRV_DATA_SOURCE) {
  $portFile = Get-ChildItem "$env:LOCALAPPDATA\Microsoft\Power BI Desktop*" `
                -Filter "msmdsrv.port.txt" -Recurse -ErrorAction SilentlyContinue |
              Sort-Object LastWriteTime -Descending | Select-Object -First 1
  if (-not $portFile -or $portFile.Length -eq 0) {
    Write-Error "no msmdsrv.port.txt — open a .pbix in Desktop, or set MSMDSRV_PORT / MSMDSRV_DATA_SOURCE"
  }
  # Leave MSMDSRV_PORT unset so the pump re-reads the file per request
  # (Desktop can restart and move the port). Print the current value so
  # the operator can see we found something.
  $preview = (Get-Content -LiteralPath $portFile.FullName -Encoding Unicode).Trim()
  Write-Host "Desktop port file $($portFile.FullName) currently $preview (re-read per request)"
}

$env:MSMDSRV_PUMP_ADDR = $Listen
dotnet run --project (Join-Path $here "pump") -c Release --no-launch-profile
