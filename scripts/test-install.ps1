# Exercises install.ps1 against a GoReleaser render in dist/, served over loopback the way
# GitHub Releases would serve it. Runs in CI (install-script-windows job) after a tagged
# dry run, as scripts/test-install.sh does for install.sh.
#
#   pwsh -NoProfile -File scripts/test-install.ps1 <dist-dir> <tag>
#
# Covers what Terma Desktop and a person run: `irm <url> | iex` under Windows PowerShell 5.1
# and PowerShell 7, pinned and latest; the default destination
# (%LOCALAPPDATA%\Programs\terma) and the user PATH it adds, once, keeping the entries
# already there unexpanded and never touching the machine's; a reinstall over a terma that
# is running; the opt-out and a folder PATH cannot hold; and the two refusals, a checksum
# that does not match and cleartext to a host that is not this machine. The user PATH is
# put back as it was.
param(
  [Parameter(Mandatory = $true)][string]$Dist,
  [Parameter(Mandatory = $true)][string]$Tag
)
Set-StrictMode -Version 3
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$root = Split-Path -Parent $PSScriptRoot
$installer = Join-Path $root 'install.ps1'
$port = if ($env:TERMA_TEST_PORT) { [int]$env:TERMA_TEST_PORT } else { 18766 }
$work = Join-Path ([IO.Path]::GetTempPath()) ('terma-test-install-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $work | Out-Null
$Dist = (Resolve-Path -LiteralPath $Dist).Path
# The release binary's version check in the dev environment, with no machine profile.
$env:TERMA_ENV = 'dev'
$env:TERMA_CONFIG_DIR = Join-Path $work 'config'
$env:TERMA_STATE_DIR = Join-Path $work 'state'
foreach ($name in 'TERMA_INSTALL_DIR', 'TERMA_NO_MODIFY_PATH', 'TERMA_VERSION', 'TERMA_RELEASE_BASE') {
  Remove-Item -LiteralPath "Env:$name" -ErrorAction SilentlyContinue
}

$mirrorRoot = Join-Path $work 'mirror'
$mirror = Join-Path $mirrorRoot 'miradorlabs\terma-cli\releases'
$pinnedDir = Join-Path $mirror "download\$Tag"
$latestDir = Join-Path $mirror 'latest\download'
New-Item -ItemType Directory -Force -Path $pinnedDir, $latestDir | Out-Null
Copy-Item -Path (Join-Path $Dist '*.zip'), (Join-Path $Dist 'checksums.txt') -Destination $pinnedDir
Copy-Item -Path (Join-Path $pinnedDir '*') -Destination $latestDir
# The installer itself, as terma.ai serves it, so the run is the documented one.
Copy-Item -LiteralPath $installer -Destination (Join-Path $mirrorRoot 'install.ps1')

$python = (Get-Command python -ErrorAction SilentlyContinue)
if (-not $python) { $python = Get-Command python3 }
$server = Start-Process -FilePath $python.Source -ArgumentList '-m', 'http.server', "$port", '--bind', '127.0.0.1' `
  -WorkingDirectory $mirrorRoot -NoNewWindow -PassThru `
  -RedirectStandardOutput (Join-Path $work 'http.log') -RedirectStandardError (Join-Path $work 'http.err')
$host_ = "http://127.0.0.1:$port"
$base = "$host_/miradorlabs/terma-cli/releases"
$want = "terma $($Tag.TrimStart('v')) (dev)"

# The user's PATH as the registry keeps it, restored at the end.
$envKey = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
$savedPath = $envKey.GetValue('Path', $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
$savedKind = if ($null -ne $savedPath) { $envKey.GetValueKind('Path') } else { $null }
$machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
$running = $null

function Fail([string]$why) { throw "FAIL: $why" }

function Get-UserPath {
  [string]$envKey.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
}

# Install runs the installer as the documented one-liner under shell (powershell or pwsh)
# with vars set for it alone, and returns what it printed and its exit code.
function Install([string]$shell, [hashtable]$vars, [string]$from = $work) {
  $set = @{}
  foreach ($k in $vars.Keys) {
    $set[$k] = [Environment]::GetEnvironmentVariable($k)
    [Environment]::SetEnvironmentVariable($k, $vars[$k])
  }
  Push-Location -LiteralPath $from
  # The installer's own errors arrive on stderr: read them, never stop on them.
  $ErrorActionPreference = 'Continue'
  try {
    $out = & $shell -NoProfile -ExecutionPolicy Bypass -Command "irm $host_/install.ps1 | iex" 2>&1 | ForEach-Object { "$_" }
    $code = $LASTEXITCODE
  } finally {
    Pop-Location
    foreach ($k in $set.Keys) { [Environment]::SetEnvironmentVariable($k, $set[$k]) }
  }
  [pscustomobject]@{ Out = ($out -join "`n"); Code = $code }
}

function Assert-Installed([string]$dir, $res) {
  $exe = Join-Path $dir 'terma.exe'
  if ($res.Code -ne 0) { Fail "install into $dir exited $($res.Code): $($res.Out)" }
  if (-not (Test-Path -LiteralPath $exe)) { Fail "no terma.exe in $dir`: $($res.Out)" }
  $got = & $exe version
  if ($got -ne $want) { Fail "$exe version = $got, want $want" }
}

try {
  $up = $false
  for ($i = 0; $i -lt 50 -and -not $up; $i++) {
    try { Invoke-WebRequest -UseBasicParsing -Uri "$host_/" | Out-Null; $up = $true } catch { Start-Sleep -Milliseconds 100 }
  }
  if (-not $up) { Fail 'the loopback mirror did not start' }

  Write-Host '== pinned version, irm | iex under Windows PowerShell 5.1'
  $bin1 = Join-Path $work 'bin1'
  Assert-Installed $bin1 (Install 'powershell' @{ TERMA_RELEASE_BASE = $base; TERMA_VERSION = $Tag; TERMA_INSTALL_DIR = $bin1; TERMA_NO_MODIFY_PATH = '1' })

  Write-Host '== latest, under PowerShell 7'
  $bin2 = Join-Path $work 'bin2'
  Assert-Installed $bin2 (Install 'pwsh' @{ TERMA_RELEASE_BASE = $base; TERMA_INSTALL_DIR = $bin2; TERMA_NO_MODIFY_PATH = '1' })

  Write-Host '== version without the v prefix'
  $bin3 = Join-Path $work 'bin3'
  Assert-Installed $bin3 (Install 'pwsh' @{ TERMA_RELEASE_BASE = $base; TERMA_VERSION = $Tag.TrimStart('v'); TERMA_INSTALL_DIR = $bin3; TERMA_NO_MODIFY_PATH = '1' })

  Write-Host '== a reinstall renames the new binary into place, leaving nothing staged'
  Assert-Installed $bin1 (Install 'powershell' @{ TERMA_RELEASE_BASE = $base; TERMA_INSTALL_DIR = $bin1; TERMA_NO_MODIFY_PATH = '1' })
  $left = @(Get-ChildItem -LiteralPath $bin1 -Force | ForEach-Object Name)
  if (($left -join ',') -ne 'terma.exe') { Fail "reinstall left $($left -join ', ')" }

  Write-Host '== a reinstall over a terma that is running moves it aside, twice'
  # Start-Running starts bin1's terma.exe as a terma that stays running: setup --desktop-grant
  # waits on stdin for its identity line, for auth.GrantTimeout (2 minutes), far longer than
  # these installs take. A shorter timeout, or flags that change, would end it early.
  function Start-Running {
    $psi = New-Object Diagnostics.ProcessStartInfo
    $psi.FileName = Join-Path $bin1 'terma.exe'
    $psi.Arguments = 'setup -y --desktop-grant --org 11111111-1111-4111-8111-111111111111 --team 22222222-2222-4222-8222-222222222222'
    $psi.UseShellExecute = $false
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $p = [Diagnostics.Process]::Start($psi)
    $script:running += @($p)
    Start-Sleep -Milliseconds 500
    if ($p.HasExited) { Fail "the stand-in terma exited: $($p.StandardError.ReadToEnd())" }
  }
  function Get-Aside { @(Get-ChildItem -LiteralPath $bin1 -Filter 'terma.exe*.old' -Force | ForEach-Object Name) }
  $running = @()
  Start-Running
  Assert-Installed $bin1 (Install 'pwsh' @{ TERMA_RELEASE_BASE = $base; TERMA_INSTALL_DIR = $bin1; TERMA_NO_MODIFY_PATH = '1' })
  if (-not (Test-Path -LiteralPath (Join-Path $bin1 'terma.exe.old'))) { Fail 'the running terma was not moved aside' }
  # terma.exe.old still runs, and so does the terma that replaced it: the next install moves
  # that one aside under a name of its own instead of failing on terma.exe.old.
  Start-Running
  Assert-Installed $bin1 (Install 'pwsh' @{ TERMA_RELEASE_BASE = $base; TERMA_INSTALL_DIR = $bin1; TERMA_NO_MODIFY_PATH = '1' })
  $aside = @(Get-Aside)
  if ($aside.Count -ne 2) { Fail "two running terma should be aside, found: $($aside -join ', ')" }
  foreach ($p in $running) { $p.Kill(); $p.WaitForExit() }
  $running = @()
  Assert-Installed $bin1 (Install 'pwsh' @{ TERMA_RELEASE_BASE = $base; TERMA_INSTALL_DIR = $bin1; TERMA_NO_MODIFY_PATH = '1' })
  $aside = @(Get-Aside)
  if ($aside.Count) { Fail "the next install did not remove $($aside -join ', ')" }

  Write-Host '== default: %LOCALAPPDATA%\Programs\terma, on the user PATH, entries there kept unexpanded'
  $local = Join-Path $work 'localappdata'
  $default = Join-Path $local 'Programs\terma'
  $keep = '%USERPROFILE%\terma-test-keep;C:\terma-test-other'
  $envKey.SetValue('Path', $keep, [Microsoft.Win32.RegistryValueKind]::ExpandString)
  $res = Install 'powershell' @{ TERMA_RELEASE_BASE = $base; TERMA_VERSION = $Tag; LOCALAPPDATA = $local }
  Assert-Installed $default $res
  if ((Get-UserPath) -ne "$keep;$default") { Fail "user PATH = $(Get-UserPath)" }
  if ($envKey.GetValueKind('Path') -ne [Microsoft.Win32.RegistryValueKind]::ExpandString) { Fail 'the user PATH lost its expandable kind' }
  if ($res.Out -notmatch [regex]::Escape("Added $default to your user PATH: new terminals will find terma.")) { Fail "no Added line: $($res.Out)" }
  if ($res.Out -notmatch [regex]::Escape('Next: run `terma setup`.')) { Fail "no Next line: $($res.Out)" }
  if ($res.Out -match '(?i)administrator|elevat|sudo') { Fail "the installer asked for rights: $($res.Out)" }

  Write-Host '== a second install adds nothing'
  $res = Install 'pwsh' @{ TERMA_RELEASE_BASE = $base; LOCALAPPDATA = $local }
  Assert-Installed $default $res
  if ((Get-UserPath) -ne "$keep;$default") { Fail "a reinstall changed the user PATH: $(Get-UserPath)" }
  if ($res.Out -notmatch [regex]::Escape("$default is already on PATH in new terminals.")) { Fail "a reinstall said: $($res.Out)" }

  Write-Host '== opted out, and a folder PATH cannot hold, leave the user PATH alone'
  $bin4 = Join-Path $work 'bin4'
  $res = Install 'pwsh' @{ TERMA_RELEASE_BASE = $base; TERMA_INSTALL_DIR = $bin4; TERMA_NO_MODIFY_PATH = '1' }
  Assert-Installed $bin4 $res
  if ($res.Out -notmatch 'is not on your PATH' -or $res.Out -notmatch [regex]::Escape("Next: run ``& '$bin4\terma.exe' setup``.")) { Fail "opted out, but said: $($res.Out)" }
  $semi = Join-Path $work 'a;b'
  $res = Install 'pwsh' @{ TERMA_RELEASE_BASE = $base; TERMA_INSTALL_DIR = $semi }
  Assert-Installed $semi $res
  if ($res.Out -notmatch 'cannot go on PATH') { Fail "a ';' in the folder, but said: $($res.Out)" }
  if ((Get-UserPath) -ne "$keep;$default") { Fail "the user PATH changed: $(Get-UserPath)" }

  Write-Host '== a relative TERMA_INSTALL_DIR is installed to absolute'
  $sub = Join-Path $work 'sub'
  New-Item -ItemType Directory -Path $sub | Out-Null
  Assert-Installed (Join-Path $work 'rel\bin') (Install 'pwsh' @{ TERMA_RELEASE_BASE = $base; TERMA_INSTALL_DIR = '..\rel\bin'; TERMA_NO_MODIFY_PATH = '1' } $sub)

  Write-Host '== a drive root stays the root, and goes on the user PATH as one'
  # subst lends a free letter to a folder. Trimmed to "T:", the root would go on PATH as that
  # drive's current folder.
  $driveRoot = Join-Path $work 'driveroot'
  New-Item -ItemType Directory -Path $driveRoot | Out-Null
  $letter = 'T', 'U', 'V', 'W', 'X', 'Y' | Where-Object { -not (Test-Path -LiteralPath "${_}:\") } | Select-Object -First 1
  if (-not $letter) { Fail 'no free drive letter to subst' }
  $drive = "${letter}:"
  & subst.exe $drive $driveRoot
  if ($LASTEXITCODE) { Fail "subst $drive $driveRoot failed" }
  try {
    Assert-Installed $driveRoot (Install 'pwsh' @{ TERMA_RELEASE_BASE = $base; TERMA_INSTALL_DIR = "$drive\" })
    $entries = (Get-UserPath).Split(';')
    if ($entries -notcontains "$drive\" -or $entries -contains $drive) { Fail "user PATH = $(Get-UserPath), want $drive\ on it" }
  } finally {
    & subst.exe $drive /d | Out-Null
    $envKey.SetValue('Path', "$keep;$default", [Microsoft.Win32.RegistryValueKind]::ExpandString)
  }

  Write-Host '== refuses a checksum that does not match'
  $sums = Join-Path $pinnedDir 'checksums.txt'
  $good = Get-Content -Raw -LiteralPath $sums
  Set-Content -LiteralPath $sums -NoNewline -Value ([regex]::Replace($good, '(?m)^[0-9a-f]{4}', 'dead'))
  $bin5 = Join-Path $work 'bin5'
  $res = Install 'pwsh' @{ TERMA_RELEASE_BASE = $base; TERMA_VERSION = $Tag; TERMA_INSTALL_DIR = $bin5; TERMA_NO_MODIFY_PATH = '1' }
  if ($res.Code -eq 0 -or $res.Out -notmatch 'checksum mismatch') { Fail "a tampered checksums.txt was accepted: $($res.Out)" }
  if (Test-Path -LiteralPath (Join-Path $bin5 'terma.exe')) { Fail 'installed despite the checksum mismatch' }
  Set-Content -LiteralPath $sums -NoNewline -Value $good

  Write-Host '== refuses cleartext to a host that is not loopback'
  $bin6 = Join-Path $work 'bin6'
  $res = Install 'powershell' @{ TERMA_RELEASE_BASE = 'http://example.invalid/releases'; TERMA_INSTALL_DIR = $bin6; TERMA_NO_MODIFY_PATH = '1' }
  if ($res.Code -eq 0 -or $res.Out -notmatch 'download failed') { Fail "a cleartext base was accepted: $($res.Out)" }
  if (Test-Path -LiteralPath (Join-Path $bin6 'terma.exe')) { Fail 'installed over cleartext' }

  if ([Environment]::GetEnvironmentVariable('Path', 'Machine') -ne $machinePath) { Fail 'the machine PATH changed' }
  Write-Host 'ok: install.ps1'
} finally {
  foreach ($p in @($running)) { if ($null -ne $p -and -not $p.HasExited) { $p.Kill() } }
  if ($null -ne $savedPath) { $envKey.SetValue('Path', $savedPath, $savedKind) } else { $envKey.DeleteValue('Path', $false) }
  $envKey.Dispose()
  Stop-Process -Id $server.Id -Force -ErrorAction SilentlyContinue
  Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
}
