# Install r2backup on Windows. Downloads the release binary, verifies it against
# the published checksums, and puts it on the PATH for this user.
#
# The command is `r2b`. The archive is still named r2backup_<version>_... --
# that string is what every already-installed copy looks for when it updates
# itself, so it is deliberately not renamed.
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

  # The binary was called r2backup.exe up to v0.1.6. Accept either name in the
  # archive so this script keeps working on an older release.
  $main = @('r2b.exe','r2backup.exe') | ForEach-Object { Join-Path $tmp $_ } | Where-Object { Test-Path $_ } | Select-Object -First 1
  if (-not $main) { throw 'the archive contains no r2b.exe' }
  Copy-Item $main (Join-Path $installDir 'r2b.exe') -Force

  # r2bw.exe is what a scheduled run is pointed at: it starts r2b with no
  # console window, which r2b cannot do for itself because Windows creates its
  # console before any of its code runs. Copying only the main binary would
  # install a working product whose scheduled backups pop a console window
  # every time, and `schedule` would correctly tell the user so -- for a file
  # that was in the archive all along.
  #
  # Tolerated when absent so this script still installs an older release.
  $launcher = @('r2bw.exe','r2backupw.exe') | ForEach-Object { Join-Path $tmp $_ } | Where-Object { Test-Path $_ } | Select-Object -First 1
  if ($launcher) {
    Copy-Item $launcher (Join-Path $installDir 'r2bw.exe') -Force
  }

  # Clear out the old names. Leaving them behind is worse than removing them:
  # the scheduled task holds an absolute path, so a stale r2backupw.exe would
  # go on quietly running last month's code forever while the user upgrades
  # the copy they type at.
  foreach ($old in 'r2backup.exe','r2backupw.exe') {
    $stale = Join-Path $installDir $old
    if (Test-Path $stale) { Remove-Item $stale -Force -ErrorAction SilentlyContinue }
  }

  # Add to the user's PATH if it is not already there.
  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  if ($userPath -notlike "*$installDir*") {
    [Environment]::SetEnvironmentVariable('Path', "$userPath;$installDir", 'User')
    Write-Host "Added $installDir to your PATH. Open a new terminal to pick it up."
  }

  # ...and re-point the scheduled task at the file that now exists. This is a
  # no-op on a machine with no schedule; it never creates one.
  & (Join-Path $installDir 'r2b.exe') schedule --repair 2>$null | Out-Null

  Write-Host ""
  Write-Host "Installed r2backup $tag to $installDir. The command is: r2b"
  Write-Host ""
  Write-Host "Next: r2b setup"
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
