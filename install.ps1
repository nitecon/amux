# Install or upgrade amux on Windows (x86-64) from the latest GitHub release.
#   irm https://raw.githubusercontent.com/nitecon/amux/main/install.ps1 | iex
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$repo = 'nitecon/amux'
$dir  = Join-Path $env:LOCALAPPDATA 'amux'
$guide = "https://github.com/$repo/blob/main/deploy/README.md#upgrade-an-existing-deployment"

Write-Warning "Upgrading? Stop the desktop, gateway, and headless clients and back up server state first. See $guide"

if (Get-Process -Name amux, amux-desktop -ErrorAction SilentlyContinue) {
    throw 'Stop the amux desktop, gateway, and headless client processes before installing or upgrading.'
}

$tag     = (Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest").tag_name
$base    = "https://github.com/$repo/releases/download/$tag"
$archive = "amux-windows-x86_64-$tag.zip"
Write-Host "Installing amux $tag into $dir"

# Stage next to the install so moves stay on one volume.
$tmp = Join-Path $env:LOCALAPPDATA ".amux-install-$([Guid]::NewGuid())"
$bundle = Join-Path $tmp 'bundle'
$old = Join-Path $tmp 'old'
New-Item -ItemType Directory -Path $bundle, $old | Out-Null
$keepTmp = $false
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

    # Stage and verify the whole bundle before touching the existing install.
    Expand-Archive $zip -DestinationPath $bundle
    foreach ($entry in Get-Content (Join-Path $bundle 'SHA256SUMS')) {
        $hash, $name = $entry -split '\s+', 2
        if ($hash -ne (Get-FileHash (Join-Path $bundle $name) -Algorithm SHA256).Hash) {
            throw "Bundle file $name failed checksum verification"
        }
    }

    # Move each bundle file into place, moving any existing copy aside first.
    # Other files in the install directory are untouched.
    New-Item -ItemType Directory -Path $dir -Force | Out-Null
    $installed = @()
    try {
        foreach ($file in Get-ChildItem $bundle) {
            $target = Join-Path $dir $file.Name
            if (Test-Path $target) { Move-Item $target $old }
            Move-Item $file.FullName $dir
            $installed += $target
        }
    } catch {
        try {
            foreach ($target in $installed) { Remove-Item $target -Force }
            Get-ChildItem $old | Move-Item -Destination $dir
        } catch {
            $keepTmp = $true
            throw "Install failed and rollback did not finish; previous files are in $old"
        }
        throw "Install failed; the previous amux install was restored. $_"
    }
} finally {
    if (-not $keepTmp) { Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue }
}

Write-Host "amux $tag installed. Start it with: & `"$dir\amux-desktop.exe`""
Write-Host "For a standalone server, update the service binary and start it with amux server --config PATH. See $guide"
