# test-race.ps1 â€” run db tests with the race detector (CGO required).
# Usage: powershell -File scripts/test-race.ps1 [extra go test args]
param(
    [string[]]$GoTestArgs = @()
)

$ErrorActionPreference = "Stop"

$gccCandidates = @(
    "C:\msys64\mingw64\bin\gcc.exe",
    "C:\msys64\ucrt64\bin\gcc.exe",
    "C:\mingw64\bin\gcc.exe",
    "$env:LOCALAPPDATA\Programs\mingw64\bin\gcc.exe"
)

$cc = $env:CC
if (-not $cc) {
    foreach ($c in $gccCandidates) {
        if ($c -and (Test-Path -LiteralPath $c)) { $cc = $c; break }
    }
}

if (-not $cc) {
    Write-Error "gcc not found. Install MSYS2 mingw-w64 or set CC. -race needs CGO."
    exit 1
}

$env:CGO_ENABLED = "1"
$env:CC = $cc
$gccDir = Split-Path -Parent $cc
$env:PATH = "$gccDir;$env:PATH"

Write-Host "CC=$env:CC CGO_ENABLED=$env:CGO_ENABLED"
& go test ./db/ ./wire/ -race -count=1 @GoTestArgs
exit $LASTEXITCODE
