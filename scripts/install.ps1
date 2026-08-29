# Install r2backup on Windows. Downloads the release binary, verifies it against
# the published checksums, and puts it on the PATH for this user.
$ErrorActionPreference = 'Stop'

$repo = 'saurabhhbansal/r2backup'
$installDir = if ($env:R2BACKUP_INSTALL_DIR) { $env:R2BACKUP_INSTALL_DIR }
              else { Join-Path $env:LOCALAPPDATA 'Programs\r2backup' }

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
  'AMD64' { 'amd64' }
  'ARM64' { 'arm64' }
  default { throw "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
}

Write-Host 'Finding the latest release...'
$release = Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest"
$tag     = $release.tag_name
$version = $tag.TrimStart('v')
$asset   = "r2backup_${version}_windows_${arch}.zip"
$base    = "https://github.com/$repo/releases/download/$tag"

$tmp = Join-Path $env:TEMP ("r2backup-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp -Force | Out-Null
try {
  Write-Host "Downloading $asset..."
  Invoke-WebRequest "$base/$asset" -OutFile (Join-Path $tmp $asset)
  Invoke-WebRequest "$base/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt')

  # Verify before unpacking. An unverified archive should never reach the
  # directory the user runs things from.
  $line = Select-String -Path (Join-Path $tmp 'checksums.txt') -Pattern ([regex]::Escape($asset)) | Select-Object -First 1
  if (-not $line) { throw "$asset is not listed in checksums.txt" }
  $expected = ($line.Line -split '\s+')[0]
  $actual   = (Get-FileHash (Join-Path $tmp $asset) -Algorithm SHA256).Hash
  if ($actual -ne $expected) { throw 'Checksum mismatch - refusing to install' }

  New-Item -ItemType Directory -Path $installDir -Force | Out-Null
  Expand-Archive -Path (Join-Path $tmp $asset) -DestinationPath $tmp -Force
  Copy-Item (Join-Path $tmp 'r2backup.exe') (Join-Path $installDir 'r2backup.exe') -Force

  # r2backupw.exe is what a scheduled run is pointed at: it starts r2backup
  # with no console window, which r2backup cannot do for itself because
  # Windows creates its console before any of its code runs. Copying only
  # r2backup.exe would install a working product whose scheduled backups pop a
  # console window every time, and `schedule` would correctly tell the user so
  # -- for a file that was in the archive all along.
  #
  # Tolerated when absent so this script still installs an older release.
  $launcher = Join-Path $tmp 'r2backupw.exe'
  if (Test-Path $launcher) {
    Copy-Item $launcher (Join-Path $installDir 'r2backupw.exe') -Force
  }

  # Add to the user's PATH if it is not already there.
  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  if ($userPath -notlike "*$installDir*") {
    [Environment]::SetEnvironmentVariable('Path', "$userPath;$installDir", 'User')
    Write-Host "Added $installDir to your PATH. Open a new terminal to pick it up."
  }

  Write-Host ""
  Write-Host "Installed r2backup $tag to $installDir"
  Write-Host ""
  Write-Host "Next: r2backup setup"
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
