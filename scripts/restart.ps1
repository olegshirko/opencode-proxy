# Restart the scheduled proxy without cutting a streaming response in half.
# Mirror of scripts/restart.sh: wait for idle, graceful stop via
# POST /admin/shutdown (the same drain SIGTERM drives on POSIX), then run the
# task again and wait for a healthy answer.
$ErrorActionPreference = "Stop"

$TaskName   = "opencode-proxy"
$LiveConfig = Join-Path $env:APPDATA "opencode-proxy\config.json"
if (-not (Test-Path $LiveConfig)) {
    throw "restart: $LiveConfig not found; run scripts\install.ps1 first"
}
$cfg = Get-Content $LiveConfig -Raw | ConvertFrom-Json
$port = ($cfg.listen -split ":")[-1]
$base = "http://127.0.0.1:$port"
$headers = @{}
if ($cfg.admin.token) { $headers["X-Admin-Token"] = $cfg.admin.token }

function Get-ProxyStatus {
    try { return Invoke-RestMethod -Uri "$base/admin/status" -Headers $headers -TimeoutSec 2 }
    catch { return $null }
}

# A restart is only safe while nothing is streaming: the drain cannot outlast
# a request that is itself waiting for this script to return.
for ($i = 0; $i -lt 120; $i++) {
    $s = Get-ProxyStatus
    if (-not $s -or $s.in_flight -eq 0) { break }
    Start-Sleep 1
}

if (Get-ProxyStatus) {
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

schtasks /Run /TN $TaskName | Out-Null
if ($LASTEXITCODE -ne 0) { throw "restart: schtasks /Run failed" }
for ($i = 0; $i -lt 400; $i++) {
    $s = Get-ProxyStatus
    if ($s -and $s.ok) {
        Write-Host "restart: healthy"
        exit 0
    }
    Start-Sleep -Milliseconds 100
}
throw "restart: proxy did not come back healthy"