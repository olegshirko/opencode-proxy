# Smoke-test a freshly built binary before it replaces the running one.
# Mirror of scripts/smoke.sh: the candidate starts on a spare port with the
# real config; the running proxy keeps serving. Nothing is swapped unless
# /admin/status answers ok:true.
param(
    [string]$Bin = ".\bin\opencode-proxy.new.exe",
    [string]$Config = (Join-Path $env:APPDATA "opencode-proxy\config.json"),
    [int]$Port = 4010
)
$ErrorActionPreference = "Stop"

$outLog = Join-Path $env:TEMP "opencode-proxy-smoke.out.log"
$errLog = Join-Path $env:TEMP "opencode-proxy-smoke.err.log"
$proc = Start-Process -FilePath $Bin `
    -ArgumentList @("-config", "`"$Config`"", "-addr", "127.0.0.1:$Port") `
    -PassThru -NoNewWindow `
    -RedirectStandardOutput $outLog -RedirectStandardError $errLog
try {
    $ok = $false
    for ($i = 0; $i -lt 60; $i++) {
        try {
            $status = Invoke-RestMethod -Uri "http://127.0.0.1:$Port/admin/status" -TimeoutSec 2
            if ($status.ok) { $ok = $true; break }
        } catch { }
        Start-Sleep -Milliseconds 100
    }
    if (-not $ok) {
        Write-Host "smoke: candidate never reported ok on port $Port"
        if (Test-Path $errLog) { Get-Content $errLog }
        exit 1
    }
    Write-Host "smoke: ok on port $Port"
}
finally {
    Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
}