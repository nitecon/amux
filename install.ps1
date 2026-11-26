# Install or upgrade amux on Windows (x86-64) from the latest GitHub release.
#   irm https://raw.githubusercontent.com/nitecon/amux/main/install.ps1 | iex
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$repo = 'nitecon/amux'
$dir  = Join-Path $env:LOCALAPPDATA 'amux'

if (Get-Process -Name amux, amux-desktop -ErrorAction SilentlyContinue) {
    throw 'Close amux before installing or upgrading it.'
}

$tag     = (Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest").tag_name
$base    = "https://github.com/$repo/releases/download/$tag"
$archive = "amux-windows-x86_64-$tag.zip"
Write-Host "Installing amux $tag into $dir"

$tmp = Join-Path ([IO.Path]::GetTempPath()) "amux-install-$([Guid]::NewGuid())"
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    $zip = Join-Path $tmp $archive
    Invoke-WebRequest "$base/$archive" -OutFile $zip -UseBasicParsing
    $sums = Join-Path $tmp 'SHA256SUMS'
    Invoke-WebRequest "$base/SHA256SUMS" -OutFile $sums -UseBasicParsing
    $line = Get-Content $sums | Where-Object { $_ -match " $([regex]::Escape($archive))$" } | Select-Object -First 1
    if (-not $line) { throw "No checksum published for $archive" }
    if (($line -split '\s+')[0] -ne (Get-FileHash $zip -Algorithm SHA256).Hash) {
        throw "Checksum mismatch for $archive"
    }

    New-Item -ItemType Directory -Path $dir -Force | Out-Null
    Expand-Archive $zip -DestinationPath $dir -Force
} finally {
    Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Host "amux $tag installed. Start it with: & `"$dir\amux-desktop.exe`""
