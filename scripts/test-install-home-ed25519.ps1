# Tests the Ed25519 check of panel/install-home.ps1, the very code of that script (taken from it, not copied): every vector
# of internal/repocheck/testdata/ed25519/vectors.json (RFC 8032, Wycheproof, ed25519-speccheck and derived cases), and, when
# -Sums and -Sig are given, a release's real SHA256SUMS and SHA256SUMS.sig against the release key in the script, which must
# verify, while the same list with one byte changed and the signature with one bit changed must not.
#
#   pwsh -NoProfile -File scripts/test-install-home-ed25519.ps1 [-Sums dist/SHA256SUMS -Sig dist/SHA256SUMS.sig]
#
# It runs on Windows PowerShell 5.1 and on PowerShell 7 (CI runs both).
param(
  [string]$Sums = '',
  [string]$Sig = ''
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 3.0

$root = [System.IO.Path]::GetFullPath([System.IO.Path]::Combine($PSScriptRoot, '..'))
$script = [System.IO.File]::ReadAllText([System.IO.Path]::Combine($root, 'panel', 'install-home.ps1'))

# the parts of the installer under test: the ed25519 region, the PEM reader, the release key, and Fail
$m = [regex]::Match($script, '(?s)#region ed25519\r?\n.*?#endregion ed25519')
if (-not $m.Success) { throw 'install-home.ps1 has no ed25519 region' }
$region = $m.Value
$tokens = $null
$errors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseInput($script, [ref]$tokens, [ref]$errors)
if ($errors.Count -ne 0) { throw ('install-home.ps1 does not parse: ' + $errors[0]) }
$pem = $ast.Find({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq 'ConvertFrom-Ed25519Pem' }, $true)
$fail = $ast.Find({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq 'Fail' }, $true)
$key = $ast.Find({ param($n) $n -is [System.Management.Automation.Language.AssignmentStatementAst] -and $n.Left.Extent.Text -eq '$ReleaseKey' }, $true)
if (-not $pem -or -not $fail -or -not $key) { throw 'install-home.ps1 lacks ConvertFrom-Ed25519Pem, Fail or $ReleaseKey' }
. ([scriptblock]::Create($fail.Extent.Text + "`n" + $region + "`n" + $pem.Extent.Text + "`n" + $key.Extent.Text))

function ConvertFrom-Hex([string]$Hex) {
  $b = New-Object byte[] ($Hex.Length / 2)
  for ($i = 0; $i -lt $b.Length; $i++) { $b[$i] = [Convert]::ToByte($Hex.Substring(2 * $i, 2), 16) }
  return , $b
}

$failed = 0
$file = [System.IO.Path]::Combine($root, 'internal', 'repocheck', 'testdata', 'ed25519', 'vectors.json')
$vectors = ([System.IO.File]::ReadAllText($file) | ConvertFrom-Json).vectors
$sw = [System.Diagnostics.Stopwatch]::StartNew()
foreach ($v in $vectors) {
  $got = Test-Ed25519 (ConvertFrom-Hex $v.pk) (ConvertFrom-Hex $v.msg) (ConvertFrom-Hex $v.sig)
  if ($got -ne $v.valid) {
    $failed++
    Write-Output ('FAIL ' + $v.source + ' ' + $v.name + ': got ' + $got + ', want ' + $v.valid)
  }
}
Write-Output ('{0} vectors, {1} failed, {2:N1} s' -f @($vectors).Count, $failed, $sw.Elapsed.TotalSeconds)
if (@($vectors).Count -lt 200) { $failed++; Write-Output 'FAIL too few vectors' }

if ($Sums -or $Sig) {
  $releaseKey = ConvertFrom-Ed25519Pem $ReleaseKey
  $list = [System.IO.File]::ReadAllBytes($Sums)
  $signature = [System.IO.File]::ReadAllBytes($Sig)
  if (-not (Test-Ed25519 $releaseKey $list $signature)) { $failed++; Write-Output 'FAIL the release signature does not verify with the key in install-home.ps1' }
  else { Write-Output 'the release signature verifies with the key in install-home.ps1' }
  $changed = [byte[]]$list.Clone()
  $changed[0] = $changed[0] -bxor 1
  if (Test-Ed25519 $releaseKey $changed $signature) { $failed++; Write-Output 'FAIL a changed list verifies' }
  $bad = [byte[]]$signature.Clone()
  $bad[63] = $bad[63] -bxor 1
  if (Test-Ed25519 $releaseKey $list $bad) { $failed++; Write-Output 'FAIL a changed signature verifies' }
}

# the key reader takes only an Ed25519 SubjectPublicKeyInfo
foreach ($wrong in @('', 'x', "-----BEGIN PUBLIC KEY-----`nMCowBQYDK2VuAyEAjH/Smxzuzy59qWgOAVCSSbeNFzaZFqnFTgVPKQlgMIU=`n-----END PUBLIC KEY-----")) {
  $took = $true
  try { [void](ConvertFrom-Ed25519Pem $wrong) } catch { $took = $false }
  if ($took) { $failed++; Write-Output ('FAIL the key reader took ' + $wrong) }
}

if ($failed -ne 0) { exit 1 }
Write-Output 'ok'
