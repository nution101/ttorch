# orcha installer (Windows). For the full parallel experience, run orcha inside WSL2
# and use docs/install.sh there. This installs a native Windows build for basic use.
#   irm https://nution101.github.io/orcha/install.ps1 | iex
$ErrorActionPreference = "Stop"

$repo = if ($env:ORCHA_REPO) { $env:ORCHA_REPO } else { "nution101/orcha" }
$installDir = "$env:LOCALAPPDATA\orcha\bin"
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }

$release = Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest"
$version = $release.tag_name
if (-not $version) { throw "Could not determine latest release for $repo." }

$asset = "orcha-$version-windows-$arch.zip"
$base = "https://github.com/$repo/releases/download/$version"
$tmp = New-TemporaryFile | ForEach-Object { Remove-Item $_; New-Item -ItemType Directory -Path $_ }

Write-Host "Downloading orcha $version for windows/$arch..."
Invoke-WebRequest -Uri "$base/$asset" -OutFile "$tmp\$asset"
Invoke-WebRequest -Uri "$base/checksums.txt" -OutFile "$tmp\checksums.txt"

$expected = (Select-String -Path "$tmp\checksums.txt" -Pattern ([regex]::Escape($asset))).Line.Split(" ")[0]
$actual = (Get-FileHash -Algorithm SHA256 "$tmp\$asset").Hash.ToLower()
if ($expected -ne $actual) { throw "Checksum mismatch for $asset; refusing to install." }

Expand-Archive -Path "$tmp\$asset" -DestinationPath $tmp -Force
New-Item -ItemType Directory -Path $installDir -Force | Out-Null
Move-Item -Path "$tmp\orcha.exe" -Destination "$installDir\orcha.exe" -Force
Remove-Item -Recurse -Force $tmp

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$installDir*") {
  [Environment]::SetEnvironmentVariable("Path", "$userPath;$installDir", "User")
  Write-Host "Added $installDir to user PATH (restart your terminal)."
}

Write-Host "Installed orcha $version -> $installDir\orcha.exe"
& "$installDir\orcha.exe" install
Write-Host "Note: parallel tmux orchestration requires WSL2. See the README."
