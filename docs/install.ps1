# orcha installer for Windows.
#
# orcha is a Linux-native tool; on Windows it runs inside WSL2. This script bootstraps
# the install into your default WSL distribution.
#   irm https://nution101.github.io/orcha/install.ps1 | iex
$ErrorActionPreference = "Stop"

$repo = if ($env:ORCHA_REPO) { $env:ORCHA_REPO } else { "nution101/orcha" }
$parts = $repo.Split('/')
$url = "https://$($parts[0]).github.io/$($parts[1])/install.sh"

function Test-Wsl {
    try { wsl.exe --status *> $null; return ($LASTEXITCODE -eq 0) } catch { return $false }
}

if (-not (Test-Wsl)) {
    Write-Host "orcha runs inside WSL2, which was not found."
    Write-Host ""
    Write-Host "1. Install WSL2 (in an admin PowerShell):  wsl --install"
    Write-Host "2. Reboot, then open your WSL distribution and run:"
    Write-Host "     curl -fsSL $url | sh"
    exit 1
}

Write-Host "Installing orcha inside WSL2..."
wsl.exe -e bash -lc "curl -fsSL $url | sh"
Write-Host ""
Write-Host "Done. Use orcha from inside WSL (open your distribution, or 'wsl')."
