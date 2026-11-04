#requires -Version 5.1
# Rubi Home installer for Windows: the helper for a computer at home that lets Rubi control Philips Hue, bring
# Claude Code's approvals and messages to your Bots, and pair with the Rubi panel. Your Bot runs it; you type nothing.
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File install-home.ps1
#
# Installs %LOCALAPPDATA%\Rubi Home\bin\rubi-home.exe after verifying the release's signed checksums against the key
# below (Ed25519, checked with the OpenSSL of Git for Windows, taken from Git's own admin-owned folder and never from
# PATH), makes Rubi Home start at login (a Scheduled Task of your own, no administrator rights), and opens Rubi
# Home's pairing window. Running it again upgrades in place.
#
# The script is constant: nothing a person writes is put into it. The only input is the optional release tag, checked
# against a pattern before it is used. The tests set RUBI_INSTALL_TEST=1, and only then are the names RUBI_RELEASE_FEED,
# RUBI_DOWNLOAD_BASE (both must be this computer), RUBI_HOME_INSTALL and RUBI_GIT_ROOT read; -NoPairing leaves out the
# last step. Without that flag the environment cannot change where anything is downloaded from or installed to.
#
# It uses .NET directly for files, hashes, the registry, ACLs and downloads, not cmdlets that PowerShell would load from
# a module folder the user can write.
param(
  [string]$Version = '',
  [switch]$NoPairing
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 3.0
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$Repo = 'Deikus-LXXVII/rubi'
$TestMode = ($env:RUBI_INSTALL_TEST -eq '1')

$ReleaseKey = @'
-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAjH/Smxzuzy59qWgOAVCSSbeNFzaZFqnFTgVPKQlgMIU=
-----END PUBLIC KEY-----
'@

# The SIDs that may own and change the files we trust: Administrators, SYSTEM and TrustedInstaller.
$TrustedSids = @('S-1-5-32-544', 'S-1-5-18', 'S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464')
# What counts as the right to change a file or folder, and to replace what is inside a folder above it.
$WriteRights = [System.Security.AccessControl.FileSystemRights]'WriteData, AppendData, WriteExtendedAttributes, DeleteSubdirectoriesAndFiles, Delete, ChangePermissions, TakeOwnership, WriteAttributes'
$AboveRights = [System.Security.AccessControl.FileSystemRights]'DeleteSubdirectoriesAndFiles, Delete, ChangePermissions, TakeOwnership'

function Say([string]$Text) { Write-Output ('rubi-home: ' + $Text) }
function Fail([string]$Text) { throw $Text }

# The tests' environment names, read only in test mode.
function Get-TestSetting([string]$Name) {
  if (-not $TestMode) { return '' }
  $v = [Environment]::GetEnvironmentVariable($Name)
  if ($v) { return $v }
  return ''
}

function Test-Loopback([Uri]$Uri) {
  return ($Uri.Host -eq '127.0.0.1' -or $Uri.Host -eq 'localhost' -or $Uri.Host -eq '[::1]')
}

# A folder or file only an administrator can change: owned by a trusted SID, no allow rule for anyone else that lets them
# write, and no link in the way. Above is for the folders over the file: there only the rights that let someone replace
# what is below count (a folder others may add files to is still fine).
function Test-AdminOnly([string]$Path, [bool]$Above) {
  $isDir = [System.IO.Directory]::Exists($Path)
  if (-not $isDir -and -not [System.IO.File]::Exists($Path)) { return $false }
  if (([System.IO.File]::GetAttributes($Path) -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) { return $false }
  if ($isDir) { $acl = ([System.IO.DirectoryInfo]::new($Path)).GetAccessControl() } else { $acl = ([System.IO.FileInfo]::new($Path)).GetAccessControl() }
  $owner = $acl.GetOwner([System.Security.Principal.SecurityIdentifier]).Value
  if ($TrustedSids -notcontains $owner) { return $false }
  $rights = $WriteRights
  if ($Above) { $rights = $AboveRights }
  foreach ($rule in $acl.GetAccessRules($true, $true, [System.Security.Principal.SecurityIdentifier])) {
    if ($rule.AccessControlType -ne [System.Security.AccessControl.AccessControlType]::Allow) { continue }
    if (($rule.FileSystemRights -band $rights) -eq 0) { continue }
    if (($rule.PropagationFlags -band [System.Security.AccessControl.PropagationFlags]::InheritOnly) -ne 0) { continue }
    if ($TrustedSids -notcontains $rule.IdentityReference.Value) { return $false }
  }
  return $true
}

# The folders Git for Windows may be in: the machine's registry entry (both views), the Program Files folders the system
# names (also for a 32-bit PowerShell), and in test mode only RUBI_GIT_ROOT.
function Get-GitRoots {
  $override = Get-TestSetting 'RUBI_GIT_ROOT'
  if ($override) { return @($override) }
  $roots = @()
  foreach ($view in @([Microsoft.Win32.RegistryView]::Registry64, [Microsoft.Win32.RegistryView]::Registry32)) {
    $hive = [Microsoft.Win32.RegistryKey]::OpenBaseKey([Microsoft.Win32.RegistryHive]::LocalMachine, $view)
    try {
      $key = $hive.OpenSubKey('SOFTWARE\GitForWindows')
      if ($key) {
        $path = $key.GetValue('InstallPath')
        if ($path) { $roots += [string]$path }
        $key.Dispose()
      }
    } finally { $hive.Dispose() }
  }
  $roots += [System.IO.Path]::Combine([Environment]::GetFolderPath('ProgramFiles'), 'Git')
  if ($env:ProgramW6432) { $roots += [System.IO.Path]::Combine($env:ProgramW6432, 'Git') }
  return $roots
}

# The OpenSSL of Git for Windows: found in Git's install folder, never through PATH, and used only if the file, every DLL
# beside it, every folder from it up to Git's root (all of these must be admin-only) and every folder above Git's root up
# to the drive (nobody may be able to replace what is below) are. Returns the path, or an empty string.
function Find-GitOpenSsl {
  foreach ($root in (Get-GitRoots)) {
    if (-not $root -or -not [System.IO.Directory]::Exists($root)) { continue }
    $root = [System.IO.Path]::GetFullPath($root).TrimEnd('\')
    foreach ($rel in @('mingw64\bin\openssl.exe', 'clangarm64\bin\openssl.exe', 'usr\bin\openssl.exe')) {
      $exe = [System.IO.Path]::Combine($root, $rel)
      if (-not [System.IO.File]::Exists($exe)) { continue }
      # the file, then every folder up to Git's root
      $chain = @($exe)
      $up = [System.IO.Path]::GetDirectoryName($exe)
      while ($up -and $up.Length -ge $root.Length) {
        $chain += $up
        if ($up -eq $root) { break }
        $up = [System.IO.Path]::GetDirectoryName($up)
      }
      if ($chain[$chain.Count - 1] -ne $root) { continue }
      $ok = $true
      foreach ($path in $chain) {
        if (-not (Test-AdminOnly $path $false)) { $ok = $false; break }
      }
      if (-not $ok) { continue }
      # the libraries it loads from its own folder
      foreach ($dll in [System.IO.Directory]::GetFiles([System.IO.Path]::GetDirectoryName($exe), '*.dll')) {
        if (-not (Test-AdminOnly $dll $false)) { $ok = $false; break }
      }
      if (-not $ok) { continue }
      # and every folder from Git's root up to the drive's root
      $above = [System.IO.Path]::GetDirectoryName($root)
      while ($above) {
        if (-not (Test-AdminOnly $above $true)) { $ok = $false; break }
        $above = [System.IO.Path]::GetDirectoryName($above)
      }
      if ($ok) { return $exe }
    }
  }
  return ''
}

# May this address be fetched? https; in test mode also http to this computer.
function Test-FetchOk([Uri]$Uri, [bool]$AllowLoopbackHttp) {
  if ($Uri.Scheme -eq 'https') { return $true }
  return ($Uri.Scheme -eq 'http' -and $AllowLoopbackHttp -and (Test-Loopback $Uri))
}

# Downloads an address into memory. Redirects are followed here, one at a time, and one from https to http is refused.
function Get-Bytes([string]$Url, [long]$Max) {
  $current = [Uri]$Url
  $allowHttp = ($TestMode -and (Test-Loopback $current))
  for ($hop = 0; $hop -le 5; $hop++) {
    if (-not (Test-FetchOk $current $allowHttp)) { Fail ('refusing to download from ' + $current.Scheme + '://' + $current.Host) }
    $req = [System.Net.HttpWebRequest]::Create($current)
    $req.AllowAutoRedirect = $false
    $req.UserAgent = 'rubi-home-installer'
    $req.Timeout = 60000
    $req.ReadWriteTimeout = 120000
    try {
      $resp = $req.GetResponse()
    } catch [System.Net.WebException] {
      if (-not $_.Exception.Response) { throw }
      $resp = $_.Exception.Response
    }
    $next = $null
    try {
      $code = [int]$resp.StatusCode
      if ($code -ge 300 -and $code -lt 400 -and $code -ne 304) {
        $location = $resp.Headers['Location']
        if (-not $location) { Fail ('download from ' + $current.Host + ': a redirect with no address') }
        $next = New-Object System.Uri($current, $location)
        if ($current.Scheme -eq 'https' -and $next.Scheme -ne 'https') { Fail ('refusing a redirect from https to ' + $next.Scheme) }
      } elseif ($code -ne 200) {
        Fail ('download from ' + $current.Host + ' failed: HTTP ' + $code)
      } else {
        $in = $resp.GetResponseStream()
        $ms = New-Object System.IO.MemoryStream
        $buf = New-Object byte[] 81920
        while (($n = $in.Read($buf, 0, $buf.Length)) -gt 0) {
          $ms.Write($buf, 0, $n)
          if ($ms.Length -gt $Max) { Fail ('download from ' + $current.Host + ' is larger than expected') }
        }
        return , $ms.ToArray()
      }
    } finally { $resp.Dispose() }
    $current = $next
  }
  Fail 'too many redirects'
}

# Writes bytes to a new file, closes it, opens it again read-only with writers (and deleters) denied, and checks that what is
# there is what was written. The returned stream is kept open for as long as another program reads the file, so it cannot be
# changed in between.
function Open-Held([string]$Path, [byte[]]$Bytes) {
  [System.IO.File]::WriteAllBytes($Path, $Bytes)
  $held = New-Object System.IO.FileStream($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::Read)
  $back = New-Object byte[] $Bytes.Length
  $got = 0
  while ($got -lt $back.Length) {
    $n = $held.Read($back, $got, $back.Length - $got)
    if ($n -le 0) { break }
    $got += $n
  }
  $extra = $held.ReadByte()
  $same = ($got -eq $Bytes.Length -and $extra -eq -1)
  for ($i = 0; $same -and $i -lt $Bytes.Length; $i++) { if ($back[$i] -ne $Bytes[$i]) { $same = $false } }
  if (-not $same) { $held.Dispose(); Fail 'a downloaded file changed while it was being written' }
  return , $held
}

# Runs OpenSSL's signature check and says whether it succeeded: exit status 0 and its "Signature Verified Successfully". It
# runs with no OPENSSL_* variable in its environment (a configuration, module or engine named there would be loaded), with
# its own folder as the working folder and first on PATH, never the folder the downloads are in.
function Test-Signature([string]$Exe, [string]$KeyFile, [string]$DataFile, [string]$SigFile) {
  $dir = [System.IO.Path]::GetDirectoryName($Exe)
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = $Exe
  $psi.Arguments = ('pkeyutl -verify -pubin -inkey "{0}" -rawin -in "{1}" -sigfile "{2}"' -f $KeyFile, $DataFile, $SigFile)
  $psi.WorkingDirectory = $dir
  $psi.UseShellExecute = $false
  $psi.RedirectStandardOutput = $true
  $psi.RedirectStandardError = $true
  $psi.CreateNoWindow = $true
  foreach ($name in @($psi.EnvironmentVariables.Keys)) {
    if ($name -like 'OPENSSL*') { $psi.EnvironmentVariables.Remove($name) }
  }
  $psi.EnvironmentVariables['PATH'] = $dir + ';' + [Environment]::SystemDirectory
  $p = [System.Diagnostics.Process]::Start($psi)
  try {
    $out = $p.StandardOutput.ReadToEndAsync()
    $err = $p.StandardError.ReadToEndAsync()
    if (-not $p.WaitForExit(60000)) { $p.Kill(); Fail 'OpenSSL did not answer' }
    $text = $out.Result + $err.Result
    return ($p.ExitCode -eq 0 -and $text.Contains('Signature Verified Successfully'))
  } finally { $p.Dispose() }
}

# Ends a running copy of the installed program (by its path), so that the new one can be started in its place.
function Stop-RunningCopy([string]$Exe) {
  foreach ($proc in [System.Diagnostics.Process]::GetProcessesByName('rubi-home')) {
    try {
      if ($proc.MainModule.FileName -ieq $Exe) {
        $proc.Kill()
        [void]$proc.WaitForExit(10000)
      }
    } catch {
      # a process of another user, or one that has already ended
    } finally { $proc.Dispose() }
  }
}

function Main {
  if ($env:OS -ne 'Windows_NT') { Fail 'this installer is for Windows; on a Mac or Linux the Bot runs install-home.sh' }
  $cpu = $env:PROCESSOR_ARCHITEW6432
  if (-not $cpu) { $cpu = $env:PROCESSOR_ARCHITECTURE }
  switch ($cpu) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    default { Fail ('unsupported CPU: ' + $cpu) }
  }

  # 1. where from and what to install: fixed addresses (the tests' stand-ins only in test mode, and only this computer)
  $feed = 'https://rubi-panel.com/releases/latest.json'
  $base = 'https://github.com/' + $Repo + '/releases/download'
  $testFeed = Get-TestSetting 'RUBI_RELEASE_FEED'
  $testBase = Get-TestSetting 'RUBI_DOWNLOAD_BASE'
  foreach ($stand in @($testFeed, $testBase)) {
    if ($stand) {
      $uri = $null
      if (-not [Uri]::TryCreate($stand, [UriKind]::Absolute, [ref]$uri) -or -not (Test-Loopback $uri)) { Fail 'in test mode the download address must be this computer' }
    }
  }
  if ($testFeed) { $feed = $testFeed }
  if ($testBase) { $base = $testBase }
  if (-not $Version) {
    $text = [System.Text.Encoding]::UTF8.GetString((Get-Bytes $feed 1048576))
    $found = [regex]::Match($text, '"version"\s*:\s*"([^"]*)"')
    if (-not $found.Success) { Fail "couldn't find the latest release" }
    $Version = $found.Groups[1].Value
  }
  # Only a plain release tag: it becomes part of file names and addresses below.
  if ($Version -cnotmatch '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?\z') { Fail 'bad version' }
  $archive = 'rubi-home-' + $Version + '-windows-' + $arch + '.zip'

  # 2. the tool that checks the signature: Git for Windows' OpenSSL, from Git's own folder
  $openssl = Find-GitOpenSsl
  if (-not $openssl) {
    Fail ("Git for Windows is needed to check the release's signature (it provides OpenSSL), and no admin-installed copy was found. " +
      'Ask the user to allow installing Git for Windows (winget install --id Git.Git -e, or https://git-scm.com/download/win; ' +
      'Windows asks them to confirm), then run this installer again. A copy of OpenSSL from anywhere else is not used.')
  }

  # 3. download into memory; the signed list and the key go to files that stay open (writers denied) while OpenSSL reads them
  Say ('downloading Rubi Home ' + $Version + ' for windows-' + $arch)
  $release = $base + '/' + $Version
  $sumsBytes = Get-Bytes ($release + '/SHA256SUMS') 1048576
  $sigBytes = Get-Bytes ($release + '/SHA256SUMS.sig') 4096
  $zipBytes = Get-Bytes ($release + '/' + $archive) 268435456
  $tmp = [System.IO.Path]::Combine([System.IO.Path]::GetTempPath(), 'rubi-home-' + [Guid]::NewGuid().ToString('N'))
  [void][System.IO.Directory]::CreateDirectory($tmp)
  $held = @()
  try {
    $keyBytes = (New-Object System.Text.UTF8Encoding($false)).GetBytes($ReleaseKey.Trim() + "`n")
    $keyHeld = Open-Held ([System.IO.Path]::Combine($tmp, 'release-key.pem')) $keyBytes
    $held += $keyHeld
    $sumsHeld = Open-Held ([System.IO.Path]::Combine($tmp, 'SHA256SUMS')) $sumsBytes
    $held += $sumsHeld
    $sigHeld = Open-Held ([System.IO.Path]::Combine($tmp, 'SHA256SUMS.sig')) $sigBytes
    $held += $sigHeld
    if (-not (Test-Signature $openssl $keyHeld.Name $sumsHeld.Name $sigHeld.Name)) { Fail "the release signature doesn't verify; not installing" }

    # the archive's checksum, from the same bytes that are unpacked below
    $expected = ''
    foreach ($line in ([System.Text.Encoding]::UTF8.GetString($sumsBytes) -split "`n")) {
      $m = [regex]::Match($line.TrimEnd("`r"), '^([0-9a-fA-F]{64})\s+\*?(\S+)$')
      if ($m.Success -and $m.Groups[2].Value -ceq $archive) { $expected = $m.Groups[1].Value.ToLowerInvariant() }
    }
    if (-not $expected) { Fail ('no checksum for ' + $archive) }
    $hasher = [System.Security.Cryptography.SHA256]::Create()
    $actual = [BitConverter]::ToString($hasher.ComputeHash($zipBytes)).Replace('-', '').ToLowerInvariant()
    if ($actual -ne $expected) { Fail ('checksum mismatch for ' + $archive + '; not installing') }
    Say 'signature and checksum verified'

    # 4. unpack the one file we expect (by its name, never by a path from the archive) from those bytes
    $dir = [System.IO.Path]::Combine([Environment]::GetFolderPath('LocalApplicationData'), 'Rubi Home')
    $testDir = Get-TestSetting 'RUBI_HOME_INSTALL'
    if ($testDir) { $dir = $testDir }
    $bindir = [System.IO.Path]::Combine($dir, 'bin')
    [void][System.IO.Directory]::CreateDirectory($bindir)
    $new = [System.IO.Path]::Combine($bindir, 'rubi-home.exe.new')
    $bin = [System.IO.Path]::Combine($bindir, 'rubi-home.exe')
    [void][System.Reflection.Assembly]::Load('System.IO.Compression, Version=4.0.0.0, Culture=neutral, PublicKeyToken=b77a5c561934e089')
    if ([System.IO.File]::Exists($new)) { [System.IO.File]::Delete($new) }
    $za = New-Object System.IO.Compression.ZipArchive((New-Object System.IO.MemoryStream(, $zipBytes)), [System.IO.Compression.ZipArchiveMode]::Read)
    try {
      $entry = $null
      foreach ($e in $za.Entries) { if ($e.FullName -ceq 'rubi-home.exe') { $entry = $e } }
      if (-not $entry) { Fail 'the archive holds no rubi-home.exe' }
      if ($entry.Length -gt 268435456) { Fail 'rubi-home.exe in the archive is larger than expected' }
      $from = $entry.Open()
      try {
        $to = New-Object System.IO.FileStream($new, [System.IO.FileMode]::CreateNew, [System.IO.FileAccess]::Write, [System.IO.FileShare]::None)
        try { $from.CopyTo($to) } finally { $to.Dispose() }
      } finally { $from.Dispose() }
    } finally { $za.Dispose() }

    # An upgrade: a running copy is ended (by its path), then the old file is moved aside (a running program can be renamed
    # but not overwritten) and the new one moved in; if that fails the old one is put back.
    Stop-RunningCopy $bin
    $old = ''
    if ([System.IO.File]::Exists($bin)) {
      $old = $bin + '.old-' + [Guid]::NewGuid().ToString('N')
      [System.IO.File]::Move($bin, $old)
    }
    try {
      [System.IO.File]::Move($new, $bin)
    } catch {
      $why = $_.Exception.Message
      if ($old -and -not [System.IO.File]::Exists($bin)) { [System.IO.File]::Move($old, $bin) }
      [System.IO.File]::Delete($new)
      Fail ("couldn't put the new rubi-home.exe in place (" + $why + '); the old one was left as it was')
    }
    foreach ($left in [System.IO.Directory]::GetFiles($bindir, 'rubi-home.exe.old-*')) {
      try { [System.IO.File]::Delete($left) } catch { Say ('an old copy is still in use and was left: ' + $left + ' (the next run of this installer removes it)') }
    }

    # 5. start at login: Rubi Home's own call makes the Scheduled Task with the user's rights, ends the task's running copy
    # and starts the new one
    & $bin install-autostart --bin $bin
    if ($LASTEXITCODE -ne 0) { Fail "couldn't set Rubi Home up to start at login" }
    Say 'running at login (a Scheduled Task named Rubi Home)'
    Say ('installed Rubi Home ' + $Version)

    # 6. the Claude Code hook is a separate, administrator-owned copy that this installer does not touch
    $hook = [System.IO.Path]::Combine([Environment]::GetFolderPath('ProgramFiles'), 'Rubi Home\rubi-home.exe')
    if ([System.IO.File]::Exists($hook)) {
      Say 'the Claude Code hook is installed; if it is not at this version, update it in the Rubi panel (Settings, Home); Windows asks the user to confirm there.'
    } else {
      Say 'Claude Code hook (optional): not set up. To let Bots ask for approvals and message your Claude Code sessions, install the hook from the Rubi panel (Settings, Home).'
    }

    # 7. pairing: Rubi Home's window shows the PIN and the link
    if (-not $NoPairing) {
      & $bin open
      if ($LASTEXITCODE -ne 0) { Fail "Rubi Home couldn't open its pairing window" }
    }
    Say 'agents, T3 and Shortcuts are not available on Windows yet; Hue, the Claude Code hook and the sister tools are.'
  } finally {
    foreach ($h in $held) { $h.Dispose() }
    try { [System.IO.Directory]::Delete($tmp, $true) } catch { }
  }
}

try {
  Main
} catch {
  [Console]::Error.WriteLine('rubi-home: error: ' + $_.Exception.Message)
  exit 1
}
