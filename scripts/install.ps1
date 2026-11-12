# Install the proxy for the current Windows user: tests, build, smoke, then a
# Task Scheduler job that starts at logon and heals itself within 5 minutes of
# a crash. Mirror of `make install` on macOS/Linux. No administrator rights.
#
# Run from anywhere (the script cd's to the repo root):
#   powershell -ExecutionPolicy Bypass -File scripts\install.ps1
$ErrorActionPreference = "Stop"
Set-Location (Split-Path $PSScriptRoot -Parent)

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "install: Go toolchain not found in PATH"
}

$TaskName   = "opencode-proxy"
$BinDir     = Join-Path $env:LOCALAPPDATA "Programs\opencode-proxy"
$ConfigDir  = Join-Path $env:APPDATA "opencode-proxy"
$StateDir   = Join-Path $env:LOCALAPPDATA "opencode-proxy\var"
$LiveConfig = Join-Path $ConfigDir "config.json"
$Bin        = Join-Path $BinDir "opencode-proxy.exe"
$Gen        = Join-Path $BinDir "genmodels.exe"

# --- gate: nothing is replaced unless the tests pass ------------------------
go test ./cmd/... ./internal/... ./statusline
if ($LASTEXITCODE -ne 0) { throw "install: tests failed" }

New-Item -ItemType Directory -Force -Path bin, $BinDir, $ConfigDir, $StateDir | Out-Null

# windowsgui: the scheduled task must not flash a console window. Lifecycle
# logs go to $StateDir\service.log instead of stderr.
go build -ldflags "-H windowsgui" -o bin\opencode-proxy.new.exe .\cmd\opencode-proxy
if ($LASTEXITCODE -ne 0) { throw "install: build failed" }
go build -o bin\genmodels.new.exe .\cmd\genmodels
if ($LASTEXITCODE -ne 0) { throw "install: genmodels build failed" }

# The candidate must answer on a spare port before anything is swapped. The
# live config self-creates from defaults on first start.
& "$PSScriptRoot\smoke.ps1" -Bin (Resolve-Path bin\opencode-proxy.new.exe).Path -Config $LiveConfig
if ($LASTEXITCODE -ne 0) { throw "install: smoke failed" }

# --- stop the old instance: a running exe is locked on Windows --------------
$cfg = Get-Content $LiveConfig -Raw | ConvertFrom-Json
$port = ($cfg.listen -split ":")[-1]
$base = "http://127.0.0.1:$port"
$headers = @{}
if ($cfg.admin.token) { $headers["X-Admin-Token"] = $cfg.admin.token }

function Get-ProxyStatus {
    try { return Invoke-RestMethod -Uri "$base/admin/status" -Headers $headers -TimeoutSec 2 }
    catch { return $null }
}

if (Get-ProxyStatus) {
    # The drain cannot outlast a request that is itself waiting for this
    # script to return, so wait for an idle moment first.
    for ($i = 0; $i -lt 120; $i++) {
        $s = Get-ProxyStatus
        if (-not $s -or $s.in_flight -eq 0) { break }
        Start-Sleep 1
    }
    try {
        Invoke-RestMethod -Method Post -Uri "$base/admin/shutdown" -Headers $headers -TimeoutSec 5 | Out-Null
    } catch {
        # Admin disabled: after the idle wait a hard stop cannot cut a stream.
        cmd /c "schtasks /End /TN $TaskName 2>nul" | Out-Null
    }
    for ($i = 0; $i -lt 400; $i++) {
        if (-not (Get-ProxyStatus)) { break }
        Start-Sleep -Milliseconds 100
    }
}

Move-Item -Force bin\opencode-proxy.new.exe $Bin
Move-Item -Force bin\genmodels.new.exe $Gen
if (-not (Test-Path (Join-Path $ConfigDir "models.json"))) {
    Copy-Item models.json (Join-Path $ConfigDir "models.json")
}

# --- register the task -------------------------------------------------------
$user = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
$xml = Get-Content deploy\opencode-proxy.task.xml.template -Raw
$xml = $xml.Replace("__USER__", $user)
$xml = $xml.Replace("__BIN__", $Bin)
$xml = $xml.Replace("__CONFIG__", $LiveConfig)
$xml = $xml.Replace("__WORKDIR__", $ConfigDir)
$xmlPath = Join-Path $env:TEMP "opencode-proxy.task.xml"
# schtasks /Create /XML only accepts UTF-16LE.
[System.IO.File]::WriteAllText($xmlPath, $xml, [System.Text.Encoding]::Unicode)
schtasks /Create /TN $TaskName /XML $xmlPath /F | Out-Null
if ($LASTEXITCODE -ne 0) { throw "install: task registration failed" }
Remove-Item $xmlPath -ErrorAction SilentlyContinue

schtasks /Run /TN $TaskName | Out-Null
if ($LASTEXITCODE -ne 0) { throw "install: schtasks /Run failed" }
for ($i = 0; $i -lt 400; $i++) {
    $s = Get-ProxyStatus
    if ($s -and $s.ok) {
        Write-Host "installed $Bin"
        Write-Host "task '$TaskName': starts at logon, self-heals within 5 minutes"
        Write-Host "verify: Invoke-RestMethod $base/admin/status"
        exit 0
    }
    Start-Sleep -Milliseconds 100
}
throw "install: proxy did not come back healthy; see $StateDir\service.log"