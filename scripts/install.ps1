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

  # A taskbar app started from the previous install is running r2b.exe, and
  # its r2bw.exe is waiting on it. Note it now, to start the new one in its
  # place once the files are in.
  $trayWasRunning = @(Get-CimInstance Win32_Process -Filter "Name='r2b.exe'" -ErrorAction SilentlyContinue |
    Where-Object { $_.ExecutablePath -eq (Join-Path $installDir 'r2b.exe') -and $_.CommandLine -match '\stray(\s|$)' })

  # Windows will not overwrite a running .exe -- the taskbar app, or a
  # backup in progress -- but it will rename one: the running copy keeps
  # going under the new name. So a file that cannot be replaced is moved
  # aside first. r2b deletes r2b.exe.old itself the next time it starts, as
  # it does after `r2b update`; the next install clears r2bw.exe.old.
  function Install-Exe($from, $to) {
    $old = "$to.old"
    if (Test-Path $old) { Remove-Item $old -Force -ErrorAction SilentlyContinue }
    if (Test-Path $to) {
      try { Copy-Item $from $to -Force; return } catch { }
      if (Test-Path $old) { $old = "$to.$([guid]::NewGuid().ToString('N').Substring(0, 8)).old" }
      Move-Item $to $old
    }
    Copy-Item $from $to -Force
  }

  # The binary was called r2backup.exe up to v0.1.6. Accept either name in the
  # archive so this script keeps working on an older release.
  $main = @('r2b.exe','r2backup.exe') | ForEach-Object { Join-Path $tmp $_ } | Where-Object { Test-Path $_ } | Select-Object -First 1
  if (-not $main) { throw 'the archive contains no r2b.exe' }
  Install-Exe $main (Join-Path $installDir 'r2b.exe')

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
    Install-Exe $launcher (Join-Path $installDir 'r2bw.exe')
  }

  # The taskbar app, in the Start menu: without this it exists only for
  # someone who reads the docs. The shortcut runs it through r2bw.exe, so no
  # console window opens. An older release has no taskbar app -- and no r2bw
  # -- so it gets no shortcut.
  $trayCmd = $null
  $hasTray = $launcher -and ((& (Join-Path $installDir 'r2b.exe') help 2>$null) -match '^\s+tray\s')
  $shortcut = Join-Path ([Environment]::GetFolderPath('Programs')) 'r2backup.lnk'
  if ($hasTray) {
    $trayCmd = Join-Path $installDir 'r2bw.exe'
    $lnk = (New-Object -ComObject WScript.Shell).CreateShortcut($shortcut)
    $lnk.TargetPath       = $trayCmd
    $lnk.Arguments        = 'tray'
    $lnk.WorkingDirectory = $installDir
    $lnk.IconLocation     = "$trayCmd,0"
    $lnk.Description      = 'See how your backups are doing, and back up now'
    $lnk.Save()
  } elseif (Test-Path $shortcut) {
    Remove-Item $shortcut -Force -ErrorAction SilentlyContinue
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
  if ($trayCmd) {
    Write-Host "The taskbar app is in the Start menu as r2backup."
  }
  Write-Host ""
  Write-Host "Next: r2b setup"

  if ($trayCmd) {
    if ($trayWasRunning.Count -gt 0) {
      # It was running the old version: swap it for the new one.
      $trayWasRunning | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
      Start-Sleep -Milliseconds 500
      Start-Process $trayCmd -ArgumentList 'tray' -WorkingDirectory $installDir
      Write-Host "Restarted the taskbar app."
    } elseif ([Environment]::UserInteractive -and -not [Console]::IsInputRedirected) {
      $answer = $null
      try { $answer = Read-Host 'Start the taskbar app now? [Y/n]' } catch { }
      if ($null -ne $answer -and $answer -notmatch '^\s*n') {
        Start-Process $trayCmd -ArgumentList 'tray' -WorkingDirectory $installDir
        Write-Host "Started. Look for the diamond in the notification area by the clock."
      }
    }
  }
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
