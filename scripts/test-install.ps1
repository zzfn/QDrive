# Offline tests for scripts/install.ps1 (Windows PowerShell 5.1 / PowerShell 7 on Windows).
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\test-install.ps1
#
# Invoke-WebRequest is replaced by a local function that serves fixtures from a
# temp dir; the CPU is mocked through PROCESSOR_ARCHITECTURE(/W6432). No network
# access, no real release, and the real USERPROFILE/PATH are never modified.
Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

$script = Join-Path $PSScriptRoot 'install.ps1'
$root = Join-Path ([System.IO.Path]::GetTempPath()) ('qdrive-test-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $root | Out-Null
Add-Type -AssemblyName System.IO.Compression
Add-Type -AssemblyName System.IO.Compression.FileSystem

$saved = @{}
foreach ($v in 'PROCESSOR_ARCHITECTURE', 'PROCESSOR_ARCHITEW6432', 'QDRIVE_VERSION', 'QDRIVE_INSTALL_DIR', 'USERPROFILE', 'TEMP', 'TMP', 'Path', 'OS') {
  $saved[$v] = [Environment]::GetEnvironmentVariable($v, 'Process')
}
$script:pass = 0; $script:failn = 0
function Ok($m) { $script:pass++; Write-Output "ok   - $m" }
function Bad($m) { $script:failn++; Write-Output "FAIL - $m" }
function Check($cond, $m) { if ($cond) { Ok $m } else { Bad $m } }

$release = Join-Path $root 'release'
function New-Zip($path, [hashtable]$entries) {
  $fs = [System.IO.File]::Create($path)
  $zip = New-Object System.IO.Compression.ZipArchive($fs, [System.IO.Compression.ZipArchiveMode]::Create)
  try {
    foreach ($k in $entries.Keys) {
      $w = New-Object System.IO.StreamWriter($zip.CreateEntry($k).Open())
      $w.Write($entries[$k]); $w.Dispose()
    }
  } finally { $zip.Dispose(); $fs.Dispose() }
}
function Write-Sum($dir, $asset, $name) {
  $h = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $dir $asset)).Hash.ToLowerInvariant()
  [System.IO.File]::WriteAllText((Join-Path $dir "$asset.sha256"), "$h  $name`n")
}
function New-Release($tag, $arch, [hashtable]$entries) {
  $d = Join-Path $release $tag; New-Item -ItemType Directory -Force -Path $d | Out-Null
  $a = "qdrive-windows-$arch.zip"
  New-Zip (Join-Path $d $a) $entries
  Write-Sum $d $a $a
}
New-Release 'latest' 'amd64' @{ 'qdrive.exe' = 'MZ latest-amd64' }
New-Release 'latest' 'arm64' @{ 'qdrive.exe' = 'MZ latest-arm64' }
New-Release 'v1.2.3' 'amd64' @{ 'qdrive.exe' = 'MZ v123-amd64' }
New-Release 'corrupt' 'amd64' @{ 'qdrive.exe' = 'MZ good' }
Add-Content -LiteralPath (Join-Path $release 'corrupt\qdrive-windows-amd64.zip') -Value 'tampered'
New-Release 'wrongname' 'amd64' @{ 'qdrive.exe' = 'MZ good' }
Write-Sum (Join-Path $release 'wrongname') 'qdrive-windows-amd64.zip' 'qdrive-windows-arm64.zip'
New-Release 'twolines' 'amd64' @{ 'qdrive.exe' = 'MZ good' }
Add-Content -LiteralPath (Join-Path $release 'twolines\qdrive-windows-amd64.zip.sha256') -Value ('0' * 64 + '  qdrive-windows-amd64.zip')
New-Release 'nosum' 'amd64' @{ 'qdrive.exe' = 'MZ good' }
Remove-Item -LiteralPath (Join-Path $release 'nosum\qdrive-windows-amd64.zip.sha256')
New-Release 'slip' 'amd64' @{ '../../evil.ps1' = 'evil'; 'sub/qdrive.exe' = 'MZ nested' }
New-Release 'noexe' 'amd64' @{ 'qdrive.exe' = 'not a PE file' }
New-Release 'partial' 'amd64' @{ 'qdrive.exe' = 'MZ good' }

$log = Join-Path $root 'requests.log'
# Mock: shadows the cmdlet (functions win over cmdlets in command lookup).
function Invoke-WebRequest {
  param([string]$Uri, [string]$OutFile, [switch]$UseBasicParsing)
  Add-Content -LiteralPath $log -Value $Uri
  $prefix = 'https://github.com/zzfn/QDrive/releases/'
  if ($Uri.StartsWith($prefix + 'latest/download/')) { $tag = 'latest' }
  elseif ($Uri.StartsWith($prefix + 'download/')) { $tag = $Uri.Substring(($prefix + 'download/').Length).Split('/')[0] }
  else { throw "mock: unexpected url $Uri" }
  $name = $Uri.Substring($Uri.LastIndexOf('/') + 1)
  if ($tag -eq 'partial') {  # simulate an interrupted transfer
    [System.IO.File]::WriteAllText($OutFile, 'half'); throw 'mock: connection reset'
  }
  $src = Join-Path (Join-Path $release $tag) $name
  if (-not (Test-Path -LiteralPath $src)) { throw "mock: 404 $Uri" }
  Copy-Item -LiteralPath $src -Destination $OutFile
}

$n = 0
function Invoke-Case {
  param([string]$Arch = 'AMD64', [string]$W6432 = '', [string]$Version = '', [string]$Bin, [switch]$Iex, [string]$PathVar = 'C:\Windows')
  $script:n++
  $tmp = Join-Path $root "tmp$script:n"; New-Item -ItemType Directory -Force -Path $tmp | Out-Null
  $env:TEMP = $tmp; $env:TMP = $tmp; $env:OS = 'Windows_NT'
  $env:PROCESSOR_ARCHITECTURE = $Arch; $env:PROCESSOR_ARCHITEW6432 = $W6432
  $env:QDRIVE_VERSION = $Version; $env:QDRIVE_INSTALL_DIR = $Bin
  $env:USERPROFILE = Join-Path $root "home$script:n"; $env:Path = $PathVar
  if (Test-Path -LiteralPath $log) { Remove-Item -LiteralPath $log }
  $r = [pscustomobject]@{ Ok = $true; Out = ''; Err = ''; Tmp = $tmp; Home = $env:USERPROFILE }
  try {
    if ($Iex) { $r.Out = (Get-Content -Raw -LiteralPath $script | Invoke-Expression 6>&1 | Out-String) }
    else { $r.Out = (& $script 6>&1 | Out-String) }
  } catch { $r.Ok = $false; $r.Err = $_.Exception.Message }
  $r
}
function Content($p) { if (Test-Path -LiteralPath $p) { [System.IO.File]::ReadAllText($p) } else { $null } }
function TmpClean($r, $bin) {
  @(Get-ChildItem -Force -LiteralPath $r.Tmp).Count -eq 0 -and
  (-not (Test-Path -LiteralPath $bin) -or @(Get-ChildItem -Force -LiteralPath $bin -Filter '.qdrive.*').Count -eq 0)
}

try {
  # Default directory: %USERPROFILE%\.local\bin, latest amd64.
  $r = Invoke-Case
  $def = Join-Path $r.Home '.local\bin\qdrive.exe'
  Check ($r.Ok -and (Content $def) -eq 'MZ latest-amd64') "latest amd64 installs to USERPROFILE\.local\bin ($($r.Err))"
  Check ((Get-Content $log)[0] -eq 'https://github.com/zzfn/QDrive/releases/latest/download/qdrive-windows-amd64.zip') 'latest URL'
  Check ($r.Out -match 'not in your PATH') 'PATH hint printed'
  Check ($env:Path -eq 'C:\Windows') 'PATH not modified'
  Check (TmpClean $r (Split-Path $def)) 'temp files cleaned on success'

  $b = Join-Path $root 'bin-arm'
  $r = Invoke-Case -Arch 'ARM64' -Bin $b -PathVar "C:\x;$b\"
  Check ($r.Ok -and (Content "$b\qdrive.exe") -eq 'MZ latest-arm64') 'arm64 installs'
  Check ($r.Out -notmatch 'not in your PATH') 'no PATH hint when already on PATH'

  $b = Join-Path $root 'bin-wow'
  $r = Invoke-Case -Arch 'x86' -W6432 'AMD64' -Bin $b
  Check ($r.Ok -and (Content "$b\qdrive.exe") -eq 'MZ latest-amd64') '32-bit host on amd64 uses PROCESSOR_ARCHITEW6432'

  $b = Join-Path $root 'bin-x86'
  $r = Invoke-Case -Arch 'x86' -Bin $b
  Check ((-not $r.Ok) -and $r.Err -match 'unsupported architecture' -and -not (Test-Path $log)) 'x86 rejected before download'

  # Pinned tag, replacing an existing binary; sibling files untouched; via iex.
  $b = Join-Path $root 'bin-tag'; New-Item -ItemType Directory -Path $b | Out-Null
  Set-Content -LiteralPath "$b\qdrive.exe" -Value 'old' -NoNewline
  Set-Content -LiteralPath "$b\other.txt" -Value 'keep' -NoNewline
  $r = Invoke-Case -Version 'v1.2.3' -Bin $b -Iex
  Check ($r.Ok -and (Content "$b\qdrive.exe") -eq 'MZ v123-amd64') "QDRIVE_VERSION tag via iex replaces binary ($($r.Err))"
  Check ((Get-Content $log)[0] -eq 'https://github.com/zzfn/QDrive/releases/download/v1.2.3/qdrive-windows-amd64.zip') 'tag URL'
  Check ((Content "$b\other.txt") -eq 'keep') 'other files untouched'
  Check (-not (Get-Variable -Name asset -Scope Global -ErrorAction SilentlyContinue)) 'iex leaks no variables'

  # Failures must keep the existing binary and leave no temp files.
  $cases = [ordered]@{
    'corrupt' = 'checksum verification failed'; 'wrongname' = 'invalid checksum file'
    'twolines' = 'invalid checksum file'; 'nosum' = 'checksum download failed'
    'slip' = 'does not contain exactly one'; 'noexe' = 'not a Windows executable'
    'partial' = 'download failed'; 'missing' = 'download failed'
  }
  foreach ($tag in $cases.Keys) {
    $b = Join-Path $root "bin-$tag"; New-Item -ItemType Directory -Path $b | Out-Null
    Set-Content -LiteralPath "$b\qdrive.exe" -Value 'old' -NoNewline
    $r = Invoke-Case -Version $tag -Bin $b
    Check ((-not $r.Ok) -and $r.Err -match $cases[$tag] -and (Content "$b\qdrive.exe") -eq 'old') "$tag fails, existing qdrive.exe kept ($($r.Err))"
    Check (TmpClean $r $b) "$tag leaves no temp files"
    Check (@(Get-ChildItem -Force -LiteralPath $b).Count -eq 1) "$tag writes nothing else into install dir"
  }
  Check (-not (Test-Path (Join-Path $root 'evil.ps1'))) 'zip-slip entry never written'

  foreach ($bad in '../x', 'v1;calc', 'v1/../../x', 'a b', '$(calc)', 'v1?x=1') {
    $b = Join-Path $root 'bin-inj'
    $r = Invoke-Case -Version $bad -Bin $b
    Check ((-not $r.Ok) -and $r.Err -match 'invalid QDRIVE_VERSION' -and -not (Test-Path $log) -and -not (Test-Path $b)) "rejects QDRIVE_VERSION '$bad'"
  }

  $b = Join-Path $root 'bin-dir'; New-Item -ItemType Directory -Path "$b\qdrive.exe" | Out-Null
  $r = Invoke-Case -Bin $b
  Check ((-not $r.Ok) -and $r.Err -match 'is a directory' -and (Test-Path "$b\qdrive.exe" -PathType Container)) 'target directory refused'

  $env:OS = ''
  try { & $script 6>$null; Bad 'non-Windows rejected' } catch { Check ($_.Exception.Message -match 'Windows only') 'non-Windows rejected' }
} finally {
  foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k], 'Process') }
  Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Output "$script:pass passed, $script:failn failed"
if ($script:failn -ne 0) { exit 1 }
