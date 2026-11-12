# Remove the scheduled task and the installed binaries. Config and state are
# kept, as on POSIX. Best-effort: missing pieces are not an error.
$ErrorActionPreference = "Continue"

$TaskName   = "opencode-proxy"
$BinDir     = Join-Path $env:LOCALAPPDATA "Programs\opencode-proxy"
$LiveConfig = Join-Path $env:APPDATA "opencode-proxy\config.json"

# Graceful stop first when the proxy answers, so the stores get their final
# flush; schtasks /End below is the fallback.
if (Test-Path $LiveConfig) {
    $cfg = Get-Content $LiveConfig -Raw | ConvertFrom-Json
    $port = ($cfg.listen -split ":")[-1]
    $headers = @{}
    if ($cfg.admin.token) { $headers["X-Admin-Token"] = $cfg.admin.token }
    try {
        Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:$port/admin/shutdown" -Headers $headers -TimeoutSec 5 | Out-Null
        Start-Sleep 2
    } catch { }
}

cmd /c "schtasks /End /TN $TaskName 2>nul" | Out-Null
cmd /c "schtasks /Delete /TN $TaskName /F 2>nul" | Out-Null
if (Test-Path $BinDir) {
    Remove-Item -Recurse -Force $BinDir
}
Write-Host "removed task '$TaskName' and $BinDir; config and state were kept"