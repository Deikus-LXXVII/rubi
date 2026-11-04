#requires -Version 5.1
# Rubi Home installer for Windows: the helper for a computer at home that lets Rubi control Philips Hue, bring
# Claude Code's approvals and messages to your Bots, and pair with the Rubi panel. Your Bot runs it; you type nothing.
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File install-home.ps1
#
# Installs %LOCALAPPDATA%\Rubi Home\bin\rubi-home.exe after verifying the release's signed checksums against the key
# below (Ed25519, checked by this script itself: it runs no other program to do it, so it needs nothing installed and
# nothing on PATH can stand in), makes Rubi Home start at login (a Scheduled Task of your own, no administrator rights), and opens Rubi
# Home's pairing window. Running it again upgrades in place.
#
# The script is constant: nothing a person writes is put into it. The only input is the optional release tag, checked
# against a pattern before it is used. The tests set RUBI_INSTALL_TEST=1, and only then are the names RUBI_RELEASE_FEED,
# RUBI_DOWNLOAD_BASE (both must be this computer), and RUBI_HOME_INSTALL read; -NoPairing leaves out the
# last step. Without that flag the environment cannot change where anything is downloaded from or installed to.
#
# It uses .NET directly for files, hashes, big numbers and downloads, not cmdlets that PowerShell would load from
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

function Say([string]$Text) { Write-Output ('rubi-home: ' + $Text) }
function Fail([string]$Text) { throw $Text }

#region ed25519
# Ed25519 signature check (RFC 8032, section 5.1.7), in PowerShell over .NET's BigInteger and SHA-512, so that checking the
# release needs no other program. It is strict where RFC 8032 is: the public key and R must be canonical encodings of curve
# points (y below p; no "negative zero" x), and S must be below L. The check is the cofactorless one, [S]B = R + [k]A,
# done by encoding [S]B - [k]A and comparing the bytes with R. Only public data passes through it, so it need not run in
# constant time. The tests (internal/repocheck/testdata/ed25519) run it on the RFC 8032 and Wycheproof vectors.
[void][System.Reflection.Assembly]::Load('System.Numerics, Version=4.0.0.0, Culture=neutral, PublicKeyToken=b77a5c561934e089')
$EdP = [System.Numerics.BigInteger]::Pow(2, 255) - 19
$EdL = [System.Numerics.BigInteger]::Pow(2, 252) + [System.Numerics.BigInteger]::Parse('27742317777372353535851937790883648493')
$EdD = [System.Numerics.BigInteger]::Remainder(($EdP - 121665) * [System.Numerics.BigInteger]::ModPow(121666, $EdP - 2, $EdP), $EdP)
$EdD2 = [System.Numerics.BigInteger]::Remainder($EdD * 2, $EdP)
$EdSqrtM1 = [System.Numerics.BigInteger]::ModPow(2, ($EdP - 1) / 4, $EdP)

# 32 little-endian bytes as a number (always positive).
function ConvertFrom-EdBytes([byte[]]$Bytes) {
  $b = New-Object byte[] ($Bytes.Length + 1)
  [System.Array]::Copy($Bytes, $b, $Bytes.Length)
  return New-Object System.Numerics.BigInteger(, $b)
}

# A number below 2^256 as 32 little-endian bytes.
function ConvertTo-EdBytes([System.Numerics.BigInteger]$N) {
  $raw = $N.ToByteArray()
  $out = New-Object byte[] 32
  [System.Array]::Copy($raw, $out, [Math]::Min(32, $raw.Length))
  return , $out
}

# A point in extended coordinates (X, Y, Z, T), with x = X/Z, y = Y/Z, xy = T/Z; every coordinate is kept in [0, p).
function Add-EdPoint($A, $B) {
  $p = $EdP
  $a1 = [System.Numerics.BigInteger]::Remainder(($A[1] - $A[0] + $p) * ($B[1] - $B[0] + $p), $p)
  $b1 = [System.Numerics.BigInteger]::Remainder(($A[1] + $A[0]) * ($B[1] + $B[0]), $p)
  $c1 = [System.Numerics.BigInteger]::Remainder([System.Numerics.BigInteger]::Remainder($A[3] * $EdD2, $p) * $B[3], $p)
  $d1 = [System.Numerics.BigInteger]::Remainder($A[2] * 2 * $B[2], $p)
  $e = $b1 - $a1 + $p
  $f = $d1 - $c1 + $p
  $g = $d1 + $c1
  $h = $b1 + $a1
  return , @(
    [System.Numerics.BigInteger]::Remainder($e * $f, $p),
    [System.Numerics.BigInteger]::Remainder($g * $h, $p),
    [System.Numerics.BigInteger]::Remainder($f * $g, $p),
    [System.Numerics.BigInteger]::Remainder($e * $h, $p))
}

# RFC 8032, 5.1.3: the point that 32 bytes encode, or $null if they encode none (also for y >= p, and for x = 0 with the
# sign bit set).
function ConvertFrom-EdPoint([byte[]]$Bytes) {
  if ($null -eq $Bytes -or $Bytes.Length -ne 32) { return $null }
  $p = $EdP
  $yb = [byte[]]$Bytes.Clone()
  $sign = ($yb[31] -shr 7) -band 1
  $yb[31] = $yb[31] -band 0x7f
  $y = ConvertFrom-EdBytes $yb
  if ($y -ge $p) { return $null }
  $yy = [System.Numerics.BigInteger]::Remainder($y * $y, $p)
  $u = [System.Numerics.BigInteger]::Remainder($yy - 1 + $p, $p)
  $v = [System.Numerics.BigInteger]::Remainder($EdD * $yy + 1, $p)
  $v3 = [System.Numerics.BigInteger]::Remainder($v * $v * $v, $p)
  $v7 = [System.Numerics.BigInteger]::Remainder($v3 * $v3 * $v, $p)
  $x = [System.Numerics.BigInteger]::Remainder($u * $v3 * [System.Numerics.BigInteger]::ModPow([System.Numerics.BigInteger]::Remainder($u * $v7, $p), ($p - 5) / 8, $p), $p)
  $vxx = [System.Numerics.BigInteger]::Remainder($v * $x * $x, $p)
  if ($vxx -ne $u) {
    if ($vxx -ne [System.Numerics.BigInteger]::Remainder($p - $u, $p)) { return $null }
    $x = [System.Numerics.BigInteger]::Remainder($x * $EdSqrtM1, $p)
  }
  if ($x.IsZero -and $sign -eq 1) { return $null }
  if ([int]($x % 2) -ne $sign) { $x = $p - $x }
  return , @($x, $y, [System.Numerics.BigInteger]::One, [System.Numerics.BigInteger]::Remainder($x * $y, $p))
}

# RFC 8032, 5.1.2: the 32 bytes of a point.
function ConvertTo-EdPointBytes($Pt) {
  $p = $EdP
  $zi = [System.Numerics.BigInteger]::ModPow($Pt[2], $p - 2, $p)
  $x = [System.Numerics.BigInteger]::Remainder($Pt[0] * $zi, $p)
  $y = [System.Numerics.BigInteger]::Remainder($Pt[1] * $zi, $p)
  $out = ConvertTo-EdBytes $y
  $out[31] = $out[31] -bor ([byte]([int]($x % 2)) -shl 7)
  return , $out
}

# The base point B (its encoding is y = 4/5 with x positive).
$EdBase = ConvertFrom-EdPoint ([byte[]](@(0x58) + @(0x66) * 31))

# Says whether Signature (64 bytes) is PublicKey's (32 bytes) Ed25519 signature of Message.
function Test-Ed25519([byte[]]$PublicKey, [byte[]]$Message, [byte[]]$Signature) {
  if ($null -eq $PublicKey -or $null -eq $Message -or $null -eq $Signature) { return $false }
  if ($PublicKey.Length -ne 32 -or $Signature.Length -ne 64) { return $false }
  $a = ConvertFrom-EdPoint $PublicKey
  if ($null -eq $a) { return $false }
  $r = New-Object byte[] 32
  [System.Array]::Copy($Signature, 0, $r, 0, 32)
  $sb = New-Object byte[] 32
  [System.Array]::Copy($Signature, 32, $sb, 0, 32)
  $s = ConvertFrom-EdBytes $sb
  if ($s -ge $EdL) { return $false }
  # k = SHA-512(R || A || M) mod L
  $sha = [System.Security.Cryptography.SHA512]::Create()
  try {
    [void]$sha.TransformBlock($r, 0, 32, $null, 0)
    [void]$sha.TransformBlock($PublicKey, 0, 32, $null, 0)
    [void]$sha.TransformFinalBlock($Message, 0, $Message.Length)
    $k = [System.Numerics.BigInteger]::Remainder((ConvertFrom-EdBytes $sha.Hash), $EdL)
  } finally { $sha.Dispose() }
  # [S]B + [k](-A), both at once (Straus): one doubling per bit, and an addition of B, -A or B - A
  $negA = @([System.Numerics.BigInteger]::Remainder($EdP - $a[0], $EdP), $a[1], $a[2], [System.Numerics.BigInteger]::Remainder($EdP - $a[3], $EdP))
  $both = Add-EdPoint $EdBase $negA
  $sBytes = ConvertTo-EdBytes $s
  $kBytes = ConvertTo-EdBytes $k
  $q = @([System.Numerics.BigInteger]::Zero, [System.Numerics.BigInteger]::One, [System.Numerics.BigInteger]::One, [System.Numerics.BigInteger]::Zero)
  for ($i = 255; $i -ge 0; $i--) {
    $q = Add-EdPoint $q $q
    $sBit = ($sBytes[$i -shr 3] -shr ($i -band 7)) -band 1
    $kBit = ($kBytes[$i -shr 3] -shr ($i -band 7)) -band 1
    if ($sBit -and $kBit) { $q = Add-EdPoint $q $both }
    elseif ($sBit) { $q = Add-EdPoint $q $EdBase }
    elseif ($kBit) { $q = Add-EdPoint $q $negA }
  }
  $enc = ConvertTo-EdPointBytes $q
  $same = $true
  for ($i = 0; $i -lt 32; $i++) { if ($enc[$i] -ne $r[$i]) { $same = $false } }
  return $same
}
#endregion ed25519

# The 32 bytes of an Ed25519 public key in PEM (SubjectPublicKeyInfo): exactly the 12 bytes that name Ed25519, then the key.
function ConvertFrom-Ed25519Pem([string]$Pem) {
  $m = [regex]::Match($Pem.Trim(), '\A-----BEGIN PUBLIC KEY-----\s+([A-Za-z0-9+/=\s]+?)\s+-----END PUBLIC KEY-----\z')
  if (-not $m.Success) { Fail 'the release key is not a PEM public key' }
  $der = [Convert]::FromBase64String(($m.Groups[1].Value -replace '\s', ''))
  $prefix = [byte[]](0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00)
  if ($der.Length -ne 44) { Fail 'the release key is not an Ed25519 key' }
  for ($i = 0; $i -lt 12; $i++) { if ($der[$i] -ne $prefix[$i]) { Fail 'the release key is not an Ed25519 key' } }
  $key = New-Object byte[] 32
  [System.Array]::Copy($der, 12, $key, 0, 32)
  return , $key
}

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

  # 2. download into memory, and check the signed list with the release key, by this script itself (no other program)
  Say ('downloading Rubi Home ' + $Version + ' for windows-' + $arch)
  $release = $base + '/' + $Version
  $sumsBytes = Get-Bytes ($release + '/SHA256SUMS') 1048576
  $sigBytes = Get-Bytes ($release + '/SHA256SUMS.sig') 4096
  $zipBytes = Get-Bytes ($release + '/' + $archive) 268435456
  if (-not (Test-Ed25519 (ConvertFrom-Ed25519Pem $ReleaseKey) $sumsBytes $sigBytes)) { Fail "the release signature doesn't verify; not installing" }

  # the archive's checksum, from the same bytes that are unpacked below
  $expected = ''
  foreach ($line in ([System.Text.Encoding]::UTF8.GetString($sumsBytes) -split "`n")) {
    $m = [regex]::Match($line.TrimEnd("`r"), '^([0-9a-fA-F]{64})\s+\*?(\S+)$')
    if ($m.Success -and $m.Groups[2].Value -ceq $archive) { $expected = $m.Groups[1].Value.ToLowerInvariant() }
  }
  if (-not $expected) { Fail ('no checksum for ' + $archive) }
  $hasher = [System.Security.Cryptography.SHA256]::Create()
  try { $actual = [BitConverter]::ToString($hasher.ComputeHash($zipBytes)).Replace('-', '').ToLowerInvariant() } finally { $hasher.Dispose() }
  if ($actual -ne $expected) { Fail ('checksum mismatch for ' + $archive + '; not installing') }
  Say 'signature and checksum verified'

  # 3. unpack the one file we expect (by its name, never by a path from the archive) from those bytes
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

  # 4. start at login: Rubi Home's own call makes the Scheduled Task with the user's rights, ends the task's running copy
  # and starts the new one
  & $bin install-autostart --bin $bin
  if ($LASTEXITCODE -ne 0) { Fail "couldn't set Rubi Home up to start at login" }
  Say 'running at login (a Scheduled Task named Rubi Home)'
  Say ('installed Rubi Home ' + $Version)

  # 5. the Claude Code hook is a separate, administrator-owned copy that this installer does not touch
  $hook = [System.IO.Path]::Combine([Environment]::GetFolderPath('ProgramFiles'), 'Rubi Home\rubi-home.exe')
  if ([System.IO.File]::Exists($hook)) {
    Say 'the Claude Code hook is installed; if it is not at this version, update it in the Rubi panel (Settings, Home); Windows asks the user to confirm there.'
  } else {
    Say 'Claude Code hook (optional): not set up. To let Bots ask for approvals and message your Claude Code sessions, install the hook from the Rubi panel (Settings, Home).'
  }

  # 6. pairing: Rubi Home's window shows the PIN and the link
  if (-not $NoPairing) {
    & $bin open
    if ($LASTEXITCODE -ne 0) { Fail "Rubi Home couldn't open its pairing window" }
  }
  Say 'agents, T3 and Shortcuts are not available on Windows yet; Hue, the Claude Code hook and the sister tools are.'
}

try {
  Main
} catch {
  [Console]::Error.WriteLine('rubi-home: error: ' + $_.Exception.Message)
  exit 1
}
