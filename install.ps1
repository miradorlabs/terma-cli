# Installs the latest terma release (or TERMA_VERSION=vX.Y.Z) on Windows.
#
#   powershell -NoProfile -ExecutionPolicy Bypass -Command "irm https://terma.ai/install.ps1 | iex"
#
# install.sh's contract, for Windows. What it does, so you can read it before you run it:
# pick the archive for this machine from GitHub Releases, verify it against the release's
# checksums.txt, extract the single terma.exe, and place it in
# %LOCALAPPDATA%\Programs\terma; it never asks for administrator rights. When that folder
# is not on PATH, it adds it to your user PATH (never the machine's), keeping every entry
# there as it was, so new terminals find terma, and to this session's. Nothing else is
# written; nothing downloaded runs before it has been verified. Works in Windows
# PowerShell 5.1 and PowerShell 7.
#
#   TERMA_VERSION         install this release instead of the latest (v1.2.3 or 1.2.3)
#   TERMA_INSTALL_DIR     put terma.exe here instead of %LOCALAPPDATA%\Programs\terma
#   TERMA_NO_MODIFY_PATH  set, to any value, to leave your user PATH alone

# One script block, so nothing it sets outlives it in the session `iex` runs it in, and a
# failure throws rather than closing that session's window.
& {
  Set-StrictMode -Version 2
  $ErrorActionPreference = 'Stop'
  $ProgressPreference = 'SilentlyContinue'

  $repo = 'miradorlabs/terma-cli'
  # TERMA_RELEASE_BASE exists for .github/workflows/ci.yml, which serves a snapshot build
  # over loopback. Only https is accepted, but for loopback when the base is loopback.
  $base = if ($env:TERMA_RELEASE_BASE) { $env:TERMA_RELEASE_BASE.TrimEnd('/') } else { "https://github.com/$repo/releases" }

  function Say([string]$line) { Write-Host $line }
  function Fail([string]$why) { throw "install.ps1: $why" }

  $loopbackBase = $base -match '^http://(127\.0\.0\.1|localhost)(:\d+)?(/|$)'
  # Cleartext is refused, including on a redirect, except from this machine.
  function Assert-Allowed([Uri]$uri) {
    if ($uri.Scheme -eq 'https') { return }
    if ($loopbackBase -and $uri.Scheme -eq 'http' -and ($uri.Host -eq '127.0.0.1' -or $uri.Host -eq 'localhost')) { return }
    Fail "download failed: refusing $($uri.Scheme) to $($uri.Host)"
  }

  try { Add-Type -AssemblyName System.Net.Http } catch { }
  # Windows PowerShell 5.1 may not offer TLS 1.2 by default; GitHub needs it.
  [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

  # Fetch follows redirects itself, so each hop is checked, and tries three times.
  function Fetch([string]$url, [string]$outFile) {
    $handler = New-Object System.Net.Http.HttpClientHandler
    $handler.AllowAutoRedirect = $false
    $client = New-Object System.Net.Http.HttpClient($handler)
    $client.Timeout = [TimeSpan]::FromMinutes(5)
    try {
      $attempt = 0
      while ($true) {
        $attempt++
        $next = [Uri]$url
        $failure = $null
        for ($hop = 0; $hop -lt 10; $hop++) {
          Assert-Allowed $next
          try {
            $resp = $client.GetAsync($next, [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult()
          } catch {
            $failure = $_.Exception.Message
            break
          }
          $code = [int]$resp.StatusCode
          if ($code -ge 300 -and $code -lt 400 -and $null -ne $resp.Headers.Location) {
            $next = New-Object Uri($next, $resp.Headers.Location)
            $resp.Dispose()
            continue
          }
          if (-not $resp.IsSuccessStatusCode) {
            $resp.Dispose()
            if ($code -lt 500) { Fail "download failed: $url (HTTP $code)" }
            $failure = "HTTP $code"
            break
          }
          # A connection that drops mid-body is a failed attempt like any other: the next
          # one starts the file again. $null: PowerShell 7 would print the VoidTaskResult.
          $file = [IO.File]::Create($outFile)
          try {
            $null = $resp.Content.CopyToAsync($file).GetAwaiter().GetResult()
          } catch {
            $failure = $_.Exception.Message
          } finally {
            $file.Dispose(); $resp.Dispose()
          }
          if ($null -eq $failure) { return }
          break
        }
        if ($null -eq $failure) { $failure = 'too many redirects' }
        if ($attempt -ge 3) { Fail "download failed: $url ($failure)" }
        Start-Sleep -Seconds $attempt
      }
    } finally {
      $client.Dispose()
    }
  }

  $arch = $env:PROCESSOR_ARCHITEW6432
  if (-not $arch) { $arch = $env:PROCESSOR_ARCHITECTURE }
  switch ($arch) {
    'AMD64' { $arch = 'x86_64' }
    'ARM64' { $arch = 'arm64' }
    default { Fail "unsupported architecture: $arch" }
  }

  # Same naming as install.sh and `terma update` (internal/selfupdate.AssetName).
  $asset = "terma_Windows_$arch.zip"
  $version = "$env:TERMA_VERSION"
  if ($version -eq '' -or $version -eq 'latest') { $release = "$base/latest/download" }
  elseif ($version.StartsWith('v')) { $release = "$base/download/$version" }
  else { $release = "$base/download/v$version" }

  $tmp = Join-Path ([IO.Path]::GetTempPath()) ('terma-install-' + [Guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory -Path $tmp | Out-Null
  try {
    Say "Downloading $asset..."
    $zipPath = Join-Path $tmp $asset
    $sumsPath = Join-Path $tmp 'checksums.txt'
    Fetch "$release/$asset" $zipPath
    Fetch "$release/checksums.txt" $sumsPath

    # Verify before extracting anything.
    $expected = $null
    foreach ($line in [IO.File]::ReadAllLines($sumsPath)) {
      if ($line.EndsWith("  $asset")) { $expected = $line.Split(' ')[0].ToLowerInvariant() }
    }
    if (-not $expected) { Fail "checksums.txt has no entry for $asset" }
    $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $zipPath).Hash.ToLowerInvariant()
    if ($actual -ne $expected) { Fail "checksum mismatch for $asset (expected $expected, got $actual)" }

    $exeTmp = Join-Path $tmp 'terma.exe'
    try { Add-Type -AssemblyName System.IO.Compression.FileSystem } catch { }
    $zip = [IO.Compression.ZipFile]::OpenRead($zipPath)
    try {
      $entry = $zip.Entries | Where-Object { $_.FullName -eq 'terma.exe' } | Select-Object -First 1
      if ($null -eq $entry) { Fail "$asset does not contain terma.exe" }
      [IO.Compression.ZipFileExtensions]::ExtractToFile($entry, $exeTmp, $true)
    } finally {
      $zip.Dispose()
    }

    # Where to put it: an explicit TERMA_INSTALL_DIR, else %LOCALAPPDATA%\Programs\terma,
    # the user's own, so no administrator is needed and `terma update` can replace it.
    $dest = $env:TERMA_INSTALL_DIR
    if (-not $dest) {
      if (-not $env:LOCALAPPDATA) { Fail 'LOCALAPPDATA is not set (set TERMA_INSTALL_DIR to install elsewhere)' }
      $dest = Join-Path $env:LOCALAPPDATA 'Programs\terma'
    }
    # Absolute, against this session's location, as a PATH entry must be, with no trailing
    # separator but a drive root's: "D:" alone is that drive's current folder, not its root.
    $dest = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($dest).TrimEnd('\')
    if ($dest -match '^[A-Za-z]:$') { $dest += '\' }
    if (-not (Test-Path -LiteralPath $dest -PathType Container)) {
      try { New-Item -ItemType Directory -Force -Path $dest | Out-Null } catch { Fail "cannot create $dest (set TERMA_INSTALL_DIR to install elsewhere)" }
    }
    $exe = Join-Path $dest 'terma.exe'
    # Staged beside the binary and renamed into place, so nothing ever runs a half-written
    # terma. Windows will not replace a running file but lets it be renamed, so a terma
    # still running (the relay) steps aside to terma.exe.old first, as `terma update` does.
    $staged = Join-Path $dest ".terma.$PID.exe"
    # What earlier installs and updates moved aside goes, unless it is still running. One
    # that is keeps its name, and this one steps aside under a name of its own. A binary an
    # install that died staged goes too, once it is old enough not to be another install's.
    Get-ChildItem -LiteralPath $dest -Filter 'terma.exe*.old' -Force -ErrorAction SilentlyContinue |
      ForEach-Object { Remove-Item -LiteralPath $_.FullName -Force -ErrorAction SilentlyContinue }
    Get-ChildItem -LiteralPath $dest -Filter '.terma.*.exe' -Force -ErrorAction SilentlyContinue |
      Where-Object { $_.LastWriteTime -lt (Get-Date).AddHours(-1) } |
      ForEach-Object { Remove-Item -LiteralPath $_.FullName -Force -ErrorAction SilentlyContinue }
    $old = "$exe.old"
    if (Test-Path -LiteralPath $old) { $old = "$exe.$PID.old" }
    $aside = $false
    try {
      Copy-Item -LiteralPath $exeTmp -Destination $staged -Force
      if (Test-Path -LiteralPath $exe) {
        Rename-Item -LiteralPath $exe -NewName (Split-Path -Leaf $old)
        $aside = $true
      }
      try {
        Move-Item -LiteralPath $staged -Destination $exe
      } catch {
        if ($aside) { Rename-Item -LiteralPath $old -NewName 'terma.exe' -ErrorAction SilentlyContinue }
        throw
      }
    } catch {
      Remove-Item -LiteralPath $staged -Force -ErrorAction SilentlyContinue
      Fail "could not install $exe ($($_.Exception.Message))"
    }
    # The one it replaced, unless it is still running; the next install or update removes it.
    if (Test-Path -LiteralPath $old) { Remove-Item -LiteralPath $old -Force -ErrorAction SilentlyContinue }
  } finally {
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
  }

  $installed = $null
  try { $installed = & $exe version 2>$null | Select-Object -First 1 } catch { }
  if (-not $installed) { $installed = 'terma' }
  Say "Installed $installed to $exe"

  # Whether dir is an entry of the PATH value raw, its variables expanded.
  function Test-OnPath([string]$raw, [string]$dir) {
    if (-not $raw) { return $false }
    foreach ($entry in $raw.Split(';')) {
      $e = [Environment]::ExpandEnvironmentVariables($entry.Trim()).TrimEnd('\')
      if ($e -and $e -ieq $dir.TrimEnd('\')) { return $true }
    }
    return $false
  }

  # The user's PATH as the registry keeps it: unexpanded, so a %VARIABLE% in it stays one.
  $envKey = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
  if ($null -eq $envKey) { $envKey = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment') }
  try {
    $userPath = [string]$envKey.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    $next = "``terma setup``"
    # terma by its path, for when PATH will not find it, quoted by PowerShell's own escaper:
    # it doubles an apostrophe in a folder name, the curly ones too, which also end a
    # single-quoted string.
    $quotedExe = [Management.Automation.Language.CodeGeneration]::EscapeSingleQuotedStringContent($exe)
    $onPath = $true
    if ((Test-OnPath $userPath $dest) -or (Test-OnPath $machinePath $dest)) {
      Say "$dest is already on PATH in new terminals."
    } elseif ($dest.Contains(';')) {
      Say "Note: $dest cannot go on PATH, which would split it at the ';'."
      $onPath, $next = $false, "``& '$quotedExe' setup``"
    } elseif (Test-Path Env:TERMA_NO_MODIFY_PATH) {
      Say "Note: $dest is not on your PATH. Add it to your user PATH to run terma by name."
      $onPath, $next = $false, "``& '$quotedExe' setup``"
    } else {
      $kind = [Microsoft.Win32.RegistryValueKind]::ExpandString
      if ($userPath) { $kind = $envKey.GetValueKind('Path') }
      $value = if ($userPath) { $userPath.TrimEnd(';') + ';' + $dest } else { $dest }
      $envKey.SetValue('Path', $value, $kind)
      # Tell running programs, Explorer first, that the environment changed, so terminals
      # opened from now on read the new PATH. Without it they do after the next sign-in.
      try {
        if (-not ('TermaInstall.Native' -as [type])) {
          Add-Type -Namespace TermaInstall -Name Native -MemberDefinition @'
[DllImport("user32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
public static extern IntPtr SendMessageTimeout(IntPtr hWnd, uint Msg, UIntPtr wParam, string lParam, uint fuFlags, uint uTimeout, out UIntPtr lpdwResult);
'@
        }
        $result = [UIntPtr]::Zero
        [TermaInstall.Native]::SendMessageTimeout([IntPtr]0xffff, 0x1A, [UIntPtr]::Zero, 'Environment', 2, 5000, [ref]$result) | Out-Null
      } catch { }
      Say "Added $dest to your user PATH: new terminals will find terma."
    }
    # This session, too, when it is the one that ran the installer.
    if ($onPath -and -not (Test-OnPath $env:Path $dest)) { $env:Path = $env:Path.TrimEnd(';') + ';' + $dest }
  } finally {
    $envKey.Dispose()
  }
  Say "Next: run $next."
}
