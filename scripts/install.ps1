# Installs qdrive.exe from a GitHub release (Windows amd64/arm64 only).
#
#   irm https://raw.githubusercontent.com/zzfn/QDrive/main/scripts/install.ps1 | iex
#
# QDRIVE_VERSION      a release tag such as v0.1.0 (default: the latest release)
# QDRIVE_INSTALL_DIR  install directory (default: $env:USERPROFILE\.local\bin)
#
# Compatible with Windows PowerShell 5.1 and PowerShell 7+. The script never
# edits PATH or profiles, never elevates, and only ever writes qdrive.exe plus
# its own temporary files. Errors are thrown (not `exit`) so a failure under
# `| iex` does not close the user's shell. It fails if no release (or no asset
# for your architecture) has been published yet.

# Everything runs inside one script block: nothing leaks into the caller's
# session, and the full text is parsed before any action is taken.
& {
  Set-StrictMode -Version 2.0
  $ErrorActionPreference = 'Stop'
  $ProgressPreference = 'SilentlyContinue'  # Invoke-WebRequest progress is very slow on 5.1

  $repository = 'zzfn/QDrive'

  function Fail([string]$Message) { throw "qdrive: $Message" }

  if ($env:OS -ne 'Windows_NT') { Fail 'unsupported system; this installer is for Windows only (use install.sh on Linux)' }

  # A 32-bit PowerShell on a 64-bit OS reports the real CPU in PROCESSOR_ARCHITEW6432.
  $cpu = $env:PROCESSOR_ARCHITEW6432
  if ([string]::IsNullOrEmpty($cpu)) { $cpu = $env:PROCESSOR_ARCHITECTURE }
  switch -Exact ("$cpu".ToUpperInvariant()) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    default { Fail "unsupported architecture '$cpu'; only amd64 and arm64 are supported" }
  }

  $version = $env:QDRIVE_VERSION
  if ([string]::IsNullOrEmpty($version)) { $version = 'latest' }
  # Strict tag charset: blocks URL/path injection such as '../', '?', '#', spaces or quotes.
  if ($version -cnotmatch '^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$' -or $version.Contains('..')) {
    Fail "invalid QDRIVE_VERSION '$version'; expected 'latest' or a release tag such as v0.1.0"
  }

  $bin = $env:QDRIVE_INSTALL_DIR
  if ([string]::IsNullOrEmpty($bin)) {
    if ([string]::IsNullOrEmpty($env:USERPROFILE)) { Fail 'USERPROFILE is not set; set QDRIVE_INSTALL_DIR' }
    $bin = Join-Path (Join-Path $env:USERPROFILE '.local') 'bin'
  }
  $bin = [System.IO.Path]::GetFullPath($bin)
  $target = Join-Path $bin 'qdrive.exe'

  $asset = "qdrive-windows-$arch.zip"
  if ($version -eq 'latest') {
    $url = "https://github.com/$repository/releases/latest/download/$asset"
  } else {
    $url = "https://github.com/$repository/releases/download/$version/$asset"
  }

  # Windows PowerShell 5.1 may default to TLS 1.0; GitHub requires TLS 1.2+.
  try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
  } catch { }

  $work = Join-Path ([System.IO.Path]::GetTempPath()) ('qdrive-install-' + [Guid]::NewGuid().ToString('N'))
  $staged = $null
  $backup = $null
  try {
    New-Item -ItemType Directory -Path $work | Out-Null
    $archive = Join-Path $work $asset
    $sumFile = "$archive.sha256"

    Write-Output "Downloading $url"
    try { Invoke-WebRequest -Uri $url -OutFile $archive -UseBasicParsing }
    catch { Fail "download failed: $url (release $version may not be published yet; see https://github.com/$repository/releases)" }
    try { Invoke-WebRequest -Uri "$url.sha256" -OutFile $sumFile -UseBasicParsing }
    catch { Fail "checksum download failed: $url.sha256 (release $version may not be published yet)" }

    # Require exactly one '<64 hex>  <asset>' entry naming this exact archive.
    $lines = @([System.IO.File]::ReadAllLines($sumFile) | Where-Object { $_.Trim() -ne '' })
    if ($lines.Count -ne 1 -or $lines[0].Trim() -cnotmatch '^([0-9A-Fa-f]{64})[ \t]+\*?(\S+)$' -or $Matches[2] -cne $asset) {
      Fail "invalid checksum file for $asset"
    }
    $expected = $Matches[1].ToLowerInvariant()
    $sha = [System.Security.Cryptography.SHA256]::Create()
    $fs = [System.IO.File]::OpenRead($archive)
    try { $actual = -join ($sha.ComputeHash($fs) | ForEach-Object { $_.ToString('x2') }) }
    finally { $fs.Dispose(); $sha.Dispose() }
    if ($actual -ne $expected) { Fail "checksum verification failed for $asset; nothing was installed" }

    # Extract only the root-level qdrive.exe entry ourselves (no Expand-Archive):
    # no other entry is ever written, so zip-slip paths cannot escape $work.
    Add-Type -AssemblyName System.IO.Compression
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $extracted = Join-Path $work 'qdrive.exe'
    try { $zip = [System.IO.Compression.ZipFile]::OpenRead($archive) }
    catch { Fail "failed to open $asset" }
    try {
      $entries = @($zip.Entries | Where-Object { $_.FullName -ceq 'qdrive.exe' })
      if ($entries.Count -ne 1) { Fail "archive $asset does not contain exactly one qdrive.exe" }
      [System.IO.Compression.ZipFileExtensions]::ExtractToFile($entries[0], $extracted, $false)
    } finally { $zip.Dispose() }
    $head = New-Object byte[] 2
    $fs = [System.IO.File]::OpenRead($extracted)
    try { $n = $fs.Read($head, 0, 2) } finally { $fs.Dispose() }
    if ($n -ne 2 -or $head[0] -ne 0x4D -or $head[1] -ne 0x5A) { Fail "qdrive.exe in $asset is not a Windows executable" }

    try { New-Item -ItemType Directory -Path $bin -Force | Out-Null }
    catch { Fail "cannot create $bin (no elevation is used; set QDRIVE_INSTALL_DIR to a writable directory)" }
    if (Test-Path -LiteralPath $target) {
      $item = Get-Item -LiteralPath $target -Force
      if ($item.PSIsContainer) { Fail "$target is a directory" }
      if ($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) { Fail "$target is a symlink or reparse point; refusing to overwrite" }
    }

    # Stage next to the target, then swap, so a failure never leaves a broken binary.
    $staged = Join-Path $bin ('.qdrive.' + [Guid]::NewGuid().ToString('N') + '.tmp')
    try {
      [System.IO.File]::Copy($extracted, $staged, $false)
      if (Test-Path -LiteralPath $target) {
        # A real backup path also works on Windows PowerShell 5.1, where a null
        # string argument can be converted to an empty path.
        $backup = Join-Path $bin ('.qdrive.' + [Guid]::NewGuid().ToString('N') + '.bak')
        [System.IO.File]::Replace($staged, $target, $backup)
      } else { [System.IO.File]::Move($staged, $target) }
      $staged = $null
    } catch {
      Fail "cannot write $target (is qdrive running? no elevation is used; set QDRIVE_INSTALL_DIR to a writable directory)"
    }

    Write-Output "Installed qdrive ($version) to $target"
    $norm = $bin.TrimEnd('\', '/')
    $onPath = @("$env:Path".Split(';') | Where-Object { $_ -and $_.Trim().TrimEnd('\', '/') -eq $norm }).Count -gt 0
    if (-not $onPath) {
      Write-Output "Note: $bin is not in your PATH. Add it yourself, e.g.:"
      Write-Output "  [Environment]::SetEnvironmentVariable('Path', '$($bin.Replace("'", "''"));' + [Environment]::GetEnvironmentVariable('Path', 'User'), 'User')"
    }
  } finally {
    if ($staged -and (Test-Path -LiteralPath $staged)) { Remove-Item -LiteralPath $staged -Force -ErrorAction SilentlyContinue }
    # Preserve the backup if a failed replacement left the target missing.
    if ($backup -and (Test-Path -LiteralPath $target) -and (Test-Path -LiteralPath $backup)) { Remove-Item -LiteralPath $backup -Force -ErrorAction SilentlyContinue }
    if (Test-Path -LiteralPath $work) { Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue }
  }
}
