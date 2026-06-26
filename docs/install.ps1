# ttorch installer for Windows.
#
# ttorch is a Linux-native tool; on Windows it runs inside WSL2. This script bootstraps
# the install into your default WSL distribution.
#   irm https://raw.githubusercontent.com/nution101/ttorch/main/docs/install.ps1 | iex
$ErrorActionPreference = "Stop"

$repo = if ($env:TTORCH_REPO) { $env:TTORCH_REPO } else { "nution101/ttorch" }
$parts = $repo.Split('/')
$url = "https://raw.githubusercontent.com/$($parts[0])/$($parts[1])/main/docs/install.sh"

function Test-Wsl {
    try { wsl.exe --status *> $null; return ($LASTEXITCODE -eq 0) } catch { return $false }
}

if (-not (Test-Wsl)) {
    Write-Host "ttorch runs inside WSL2, which was not found."
    Write-Host ""
    Write-Host "1. Install WSL2 (in an admin PowerShell):  wsl --install"
    Write-Host "2. Reboot, then open your WSL distribution and run:"
    Write-Host "     curl -fsSL $url | sh"
    exit 1
}

Write-Host "Installing ttorch inside WSL2..."
# install.sh (run inside WSL) verifies the sha256 checksum and, when cosign is
# present in the WSL distribution, additionally cosign-verifies the release —
# so there is no separate artifact to verify on the Windows side.
wsl.exe -e bash -lc "curl -fsSL $url | sh"
Write-Host ""
Write-Host "Done. Use ttorch from inside WSL (open your distribution, or 'wsl')."
