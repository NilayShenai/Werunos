# Build script for Werunos on Windows
param(
    [switch]$Run
)

$env:CGO_CFLAGS  = "-IC:/PROGRA~2/WinFsp/inc/fuse"
$env:CGO_LDFLAGS = "-LC:/PROGRA~2/WinFsp/lib -lwinfsp-x64"

Write-Host "Building werunos.exe..." -ForegroundColor Cyan
go build -o werunos.exe .

if ($LASTEXITCODE -ne 0) {
    Write-Host "Build failed." -ForegroundColor Red
    exit 1
}

Write-Host "Build succeeded: werunos.exe" -ForegroundColor Green

if ($Run) {
    Write-Host ""
    .\werunos.exe
}
