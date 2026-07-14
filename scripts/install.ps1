param(
    [string]$InstallDir = "$env:LOCALAPPDATA\Programs\codexm",
    [string]$Version = "dev"
)

$ErrorActionPreference = "Stop"
$RootDir = Split-Path -Parent $PSScriptRoot

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "Go 1.22+ is required to build codexm from source."
}

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Push-Location $RootDir
try {
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw "Tests failed." }
    go build -trimpath -ldflags "-s -w -X main.version=$Version" -o "$InstallDir\codexm.exe" ./cmd/codexm
    if ($LASTEXITCODE -ne 0) { throw "Build failed." }
}
finally {
    Pop-Location
}

Write-Host "Installed codexm to $InstallDir\codexm.exe"
$currentUserPath = [Environment]::GetEnvironmentVariable("Path", "User")
$parts = @($currentUserPath -split ';' | Where-Object { $_ })
if ($parts -notcontains $InstallDir) {
    Write-Host "Add this directory to your user PATH: $InstallDir"
}
