# Install Power BI Desktop, open a .pbix, and report the Analysis Services port.
#
# The only half of this suite that cannot run anywhere but Windows. Everything
# it decides is reported as a STAGE line so the harness reads outcomes rather
# than exit codes: "installed but never listened" and "listened but the query
# failed" are different findings, and the whole point of the spike is learning
# WHICH happens on a GitHub runner.
#
# Silent install is Microsoft's own documented path — `-quiet ACCEPT_EULA=1`,
# with DISABLE_UPDATE_NOTIFICATION=1 so a monthly release cannot move the
# assertion under us mid-run.
#
# KNOWN RISKS, none resolvable except by running it:
#   * windows-latest is Windows SERVER 2025, and Microsoft recommends a CLIENT
#     Windows. The stated reason is IE Enhanced Security blocking sign-in to the
#     Power BI service — which this never does, opening a local file instead. So
#     the guidance applies and its rationale does not.
#   * Desktop requires >= 1440x900 and a non-system account. A hosted runner has
#     a virtual display and runs as `runneradmin`, so both are plausible and
#     neither is promised.
$ErrorActionPreference = "Stop"

param(
  [string]$PbixPath = "$PSScriptRoot\model.pbix",
  [int]$TimeoutSec = 300
)

function Stage($name, $outcome) { Write-Output "STAGE $name :: $outcome" }

# --- install -----------------------------------------------------------------
$exe = "$env:RUNNER_TEMP\PBIDesktopSetup_x64.exe"
try {
  Invoke-WebRequest -Uri "https://download.microsoft.com/download/8/8/0/880BCA75-79DD-466A-927D-1ABF1F5454B0/PBIDesktopSetup_x64.exe" `
                    -OutFile $exe -UseBasicParsing
  Stage "download" "OK ($([math]::Round((Get-Item $exe).Length / 1MB)) MB)"
} catch {
  Stage "download" "$($_.Exception.GetType().Name) :: $($_.Exception.Message)"
  exit 1
}

try {
  $p = Start-Process -FilePath $exe -Wait -PassThru `
        -ArgumentList "-quiet","-norestart","ACCEPT_EULA=1","DISABLE_UPDATE_NOTIFICATION=1"
  if ($p.ExitCode -ne 0) { Stage "install" "exit $($p.ExitCode)"; exit 1 }
  Stage "install" "OK"
} catch {
  Stage "install" "$($_.Exception.GetType().Name) :: $($_.Exception.Message)"
  exit 1
}

$pbid = Get-ChildItem "$env:ProgramFiles\Microsoft Power BI Desktop" -Filter PBIDesktop.exe `
          -Recurse -ErrorAction SilentlyContinue | Select-Object -First 1
if (-not $pbid) { Stage "locate" "PBIDesktop.exe not found after install"; exit 1 }
Stage "locate" "OK $($pbid.FullName)"

# --- launch ------------------------------------------------------------------
# Desktop hosts msmdsrv as a CHILD of the GUI process, so the GUI must actually
# start. There is no headless mode; this is the step most likely to fail on a
# runner with no real display.
try {
  $proc = Start-Process -FilePath $pbid.FullName -ArgumentList "`"$PbixPath`"" -PassThru
  Stage "launch" "OK pid=$($proc.Id)"
} catch {
  Stage "launch" "$($_.Exception.GetType().Name) :: $($_.Exception.Message)"
  exit 1
}

# --- wait for the Analysis Services port -------------------------------------
# Poll for the port file rather than sleeping a guessed span: loading 100k rows
# takes as long as it takes, and a fixed wait either truncates it or pads every
# run. Absence of the file is the signal that Desktop never got that far.
$deadline = (Get-Date).AddSeconds($TimeoutSec)
$portFile = $null
while ((Get-Date) -lt $deadline) {
  if ($proc.HasExited) { Stage "port" "Desktop exited early with $($proc.ExitCode)"; exit 1 }
  $portFile = Get-ChildItem "$env:LOCALAPPDATA\Microsoft\Power BI Desktop*" `
                -Filter "msmdsrv.port.txt" -Recurse -ErrorAction SilentlyContinue |
              Sort-Object LastWriteTime -Descending | Select-Object -First 1
  if ($portFile -and $portFile.Length -gt 0) { break }
  Start-Sleep -Seconds 2
}
if (-not $portFile -or $portFile.Length -eq 0) {
  Stage "port" "no msmdsrv.port.txt after ${TimeoutSec}s — Desktop started but never hosted Analysis Services"
  exit 1
}
Stage "port" "OK $($portFile.FullName)"
Write-Output "PORTFILE $($portFile.FullName)"
