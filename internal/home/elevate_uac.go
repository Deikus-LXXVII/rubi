package home

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"
)

// The Windows form of Elevator: a UAC prompt for the system's own PowerShell, which runs a script whose hash it checks first.
//
// What this does and does not protect (decision of the owner: no code signing, so there is no signed elevated helper):
//
//   - The program UAC elevates is the system's powershell.exe (System32, writable by administrators only), not Rubi Home's own
//     exe, which sits in a folder the user can write and could be swapped before the elevated start. The script it runs is
//     in a file in a temporary folder and the plan is in another; both are written by the user's side, so both are written by
//     something a same-user process could also write. They are therefore not trusted by where they are: the elevated command
//     line carries the SHA-256 of the script file, which a tiny bootstrap checks before it runs the script, and the script in
//     turn carries the SHA-256 of the plan file, which it checks before it uses the plan (psBoot, psInstall). Those hashes are
//     base64 and hex on the command line of the elevated process, set when it was started.
//   - What a same-user process CAN do, and nothing here prevents: it can interfere with the non-elevated side before UAC (the
//     relay, which is a PowerShell of the same user: its environment, its code, a debugger), so that the elevated process is
//     given a different command line; it can show a look-alike prompt; and it can start its own UAC request for the same
//     powershell.exe, which looks the same to the person, because UAC names the program, not the script. What the person
//     approved is therefore the plan in the panel (its hash), and what protects the computer after that is the check from outside:
//     InstallHook hashes the target, runs its version, and demands that Windows says only administrators can change it and
//     the folders above it. A signed elevated helper would let UAC show who is asking; without one, this is the best a program
//     of the user's can do, and it is no defence against malware that already runs as the user and wants to wait for a UAC
//     prompt the user will approve.
//   - The environment of the relay is minimal (minimalEnv). The environment of the elevated process is whatever Windows gives a
//     process started with the runas verb from it; values the user keeps in HKCU\Environment may still be applied by Windows.
//
// The relay is a second, non-elevated PowerShell (psLaunch) that starts the elevated one with Start-Process -Verb RunAs and
// waits for its exit code: 5 is "UAC was declined", 11 "the plan or the script changed after it was approved", 12 and 13 "a file
// is not the one approved", 14 "the target is not a Rubi file or folder", 15 "another installation is running", 16 "the hook is
// running". The result is then checked from outside, not believed.

const (
	uacDeclined    = 5
	uacPlanChanged = 11
	uacFileChanged = 12
	uacPluginFiles = 13
	uacNotRubi     = 14
	uacBusy        = 15
	uacInUse       = 16
)

// psBoot is run by the elevated PowerShell: it checks the SHA-256 of the script file and runs the script. %FILE% is the path
// of the script file in base64 and %HASH% the hash (hex, checked) of its bytes. It is short on purpose: the elevated command line
// is its base64, and ShellExecute is happier with a short one.
const psBoot = `$f=[System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String('%FILE%'))
$b=[System.IO.File]::ReadAllBytes($f)
$h=([System.BitConverter]::ToString([System.Security.Cryptography.SHA256]::Create().ComputeHash($b)) -replace '-','').ToLower()
if($h -ne '%HASH%'){exit 11}
Invoke-Expression ([System.Text.Encoding]::UTF8.GetString($b))
`

// psInstall is the script the bootstrap runs, once it has checked it. %PLAN% and %HASH% are replaced by base64 text (see
// uacInner). It has no check of the folders above its target: it creates its own folder with a protected access list, and the
// caller looks at every folder above the target from outside afterwards (verifyAdminOnly), which is the check that matters.
// The target and the plugin folder come from the plan, which has the hash the person approved.
const psInstall = `$ErrorActionPreference = 'Stop'
$mutex = New-Object System.Threading.Mutex($false, 'Global\RubiHomeInstall')
if (-not $mutex.WaitOne(0)) { exit 15 }
$planPath = [System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String('%PLAN%'))
$want = [System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String('%HASH%'))
$sha = [System.Security.Cryptography.SHA256]::Create()
function Get-Sha([byte[]]$b) { ([System.BitConverter]::ToString($sha.ComputeHash($b)) -replace '-', '').ToLower() }
function Lock-Path([string]$path, [bool]$dir) {
  $acl = Get-Acl -LiteralPath $path
  $acl.SetAccessRuleProtection($true, $false)
  foreach ($r in @($acl.Access)) { [void]$acl.RemoveAccessRule($r) }
  $inherit = if ($dir) { [System.Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit' } else { [System.Security.AccessControl.InheritanceFlags]::None }
  $none = [System.Security.AccessControl.PropagationFlags]::None
  $allow = [System.Security.AccessControl.AccessControlType]::Allow
  foreach ($e in @(@('BUILTIN\Administrators', 'FullControl'), @('NT AUTHORITY\SYSTEM', 'FullControl'), @('BUILTIN\Users', 'ReadAndExecute'))) {
    $acl.AddAccessRule((New-Object System.Security.AccessControl.FileSystemAccessRule($e[0], $e[1], $inherit, $none, $allow)))
  }
  $acl.SetOwner([System.Security.Principal.NTAccount]'BUILTIN\Administrators')
  Set-Acl -LiteralPath $path -AclObject $acl
}
try {
  $raw = [System.IO.File]::ReadAllBytes($planPath)
  if ((Get-Sha $raw) -ne $want) { exit 11 }
  $plan = [System.Text.Encoding]::UTF8.GetString($raw) | ConvertFrom-Json
  if ($plan.v -ne 1) { exit 1 }
  $b = $plan.binary
  if ((Split-Path -Leaf $b.dst) -ne 'rubi-home.exe') { exit 14 }
  if ($plan.plugin) {
    if ((Split-Path -Leaf $plan.plugin.dst) -ne 'claude-plugin') { exit 14 }
    if ((Test-Path -LiteralPath $plan.plugin.dst) -and -not (Test-Path -LiteralPath (Join-Path $plan.plugin.dst '.claude-plugin\marketplace.json'))) { exit 14 }
  }
  $bytes = [System.IO.File]::ReadAllBytes($b.src)
  if ((Get-Sha $bytes) -ne $b.sha256) { exit 12 }
  $dir = Split-Path -Parent $b.dst
  [void](New-Item -ItemType Directory -Force -LiteralPath $dir)
  Lock-Path $dir $true
  $tmp = Join-Path $dir ('.rubi-home.' + [System.Guid]::NewGuid().ToString('N'))
  [System.IO.File]::WriteAllBytes($tmp, $bytes)
  Lock-Path $tmp $false
  # A program that is running cannot be replaced on Windows: try for a few seconds (a hook of Claude Code is short), then say so.
  $moved = $false
  for ($i = 0; $i -lt 5 -and -not $moved; $i++) {
    try { Move-Item -Force -LiteralPath $tmp -Destination $b.dst; $moved = $true } catch { Start-Sleep -Seconds 1 }
  }
  if (-not $moved) { Remove-Item -Force -LiteralPath $tmp -ErrorAction SilentlyContinue; exit 16 }
  if ($plan.plugin) {
    $p = $plan.plugin
    $pdir = Split-Path -Parent $p.dst
    [void](New-Item -ItemType Directory -Force -LiteralPath $pdir)
    $new = Join-Path $pdir ('.claude-plugin.' + [System.Guid]::NewGuid().ToString('N'))
    [void](New-Item -ItemType Directory -Force -LiteralPath $new)
    foreach ($f in $p.files) {
      $data = [System.Convert]::FromBase64String($f.data)
      if ((Get-Sha $data) -ne $f.sha256) { Remove-Item -Recurse -Force -LiteralPath $new; exit 13 }
      $target = Join-Path $new ($f.path -replace '/', '\')
      [void](New-Item -ItemType Directory -Force -LiteralPath (Split-Path -Parent $target))
      [System.IO.File]::WriteAllBytes($target, $data)
    }
    Lock-Path $new $true
    foreach ($item in Get-ChildItem -LiteralPath $new -Recurse -Force) { Lock-Path $item.FullName $item.PSIsContainer }
    $old = $null
    if (Test-Path -LiteralPath $p.dst) { $old = Join-Path $pdir ('.claude-plugin.old.' + [System.Guid]::NewGuid().ToString('N')); Move-Item -LiteralPath $p.dst -Destination $old }
    try { Move-Item -LiteralPath $new -Destination $p.dst } catch { if ($old) { Move-Item -LiteralPath $old -Destination $p.dst }; throw }
    if ($old) { Remove-Item -Recurse -Force -LiteralPath $old }
  }
  exit 0
} catch {
  exit 1
}
`

// psLaunch is run by the person's own, non-elevated PowerShell: it starts the elevated one and returns its exit code. %PS% is
// the path of the system's powershell.exe and %BOOT% the base64 command of psBoot; both are filled in with characters that
// cannot end the string.
const psLaunch = `$ErrorActionPreference = 'Stop'
try {
  $p = Start-Process -FilePath '%PS%' -Verb RunAs -Wait -PassThru -WindowStyle Hidden -ArgumentList @('-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-EncodedCommand', '%BOOT%')
} catch {
  exit 5
}
exit $p.ExitCode
`

var base64Text = regexp.MustCompile(`^[A-Za-z0-9+/=]+$`)

// psEncoded is a script as PowerShell's -EncodedCommand wants it: base64 of its UTF-16 little-endian text.
func psEncoded(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 0, 2*len(u))
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	return base64.StdEncoding.EncodeToString(b)
}

// uacInner is the elevated script for a plan file and a plan hash.
func uacInner(planPath, hash string) (string, error) {
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(hash) {
		return "", errors.New("the plan's hash is not a SHA-256")
	}
	p := base64.StdEncoding.EncodeToString([]byte(planPath))
	h := base64.StdEncoding.EncodeToString([]byte(hash))
	if !base64Text.MatchString(p) || !base64Text.MatchString(h) {
		return "", errors.New("the plan's path can't be passed")
	}
	return strings.NewReplacer("%PLAN%", p, "%HASH%", h).Replace(psInstall), nil
}

// uacBoot is the elevated command's script: it checks the file of the install script against its hash and runs it.
func uacBoot(scriptPath, scriptHash string) (string, error) {
	if !hexSHA256.MatchString(scriptHash) {
		return "", errors.New("the script's hash is not a SHA-256")
	}
	f := base64.StdEncoding.EncodeToString([]byte(scriptPath))
	if !base64Text.MatchString(f) {
		return "", errors.New("the script's path can't be passed")
	}
	return strings.NewReplacer("%FILE%", f, "%HASH%", scriptHash).Replace(psBoot), nil
}

var hexSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// uacLaunch is the person's script for an elevated command (boot is the bootstrap's text).
func uacLaunch(psPath, boot string) (string, error) {
	enc := psEncoded(boot)
	if !base64Text.MatchString(enc) || strings.ContainsAny(psPath, "'\"\r\n\x00") {
		return "", errors.New("the script host's command can't be passed")
	}
	return strings.NewReplacer("%PS%", psPath, "%BOOT%", enc).Replace(psLaunch), nil
}

// minimalEnv is the environment of the relay: the system's own folders, the system's PATH, and the user's own folders that
// PowerShell and Start-Process look at. Nothing else of the user's environment (no PATH of theirs, no PSModulePath, no
// PowerShell or .NET switches in variables) is handed on. Values set in HKCU\Environment may still be applied by Windows.
func minimalEnv(systemDir string, getenv func(string) string) []string {
	root := systemDir
	if i := strings.LastIndex(systemDir, `\`); i > 0 {
		root = systemDir[:i]
	}
	drive := ""
	if len(root) >= 2 && root[1] == ':' {
		drive = root[:2]
	}
	env := []string{
		"SystemRoot=" + root, "windir=" + root, "PATH=" + systemDir + ";" + root, "ComSpec=" + systemDir + `\cmd.exe`, "PATHEXT=.COM;.EXE;.BAT;.CMD",
	}
	if drive != "" {
		env = append(env, "SystemDrive="+drive)
	}
	for _, k := range []string{"USERPROFILE", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP"} {
		if v := getenv(k); v != "" && !strings.ContainsAny(v, "\x00\r\n") {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// uacElevator makes the Windows form.
func uacElevator(env ElevateEnv) Elevator {
	return elevatorFuncs{
		describe: "UAC: a Microsoft-signed script host from the System32 folder runs a fixed bootstrap as administrator; it checks the SHA-256 of the install script, which checks that of the plan, before anything is done",
		sentence: "Windows will ask for permission to let a Microsoft program make changes on that computer (that is Rubi Home's installer); choose Yes only if you asked for this",
		available: func(context.Context) error {
			if env.SystemDir == "" {
				return fmt.Errorf("%w: the system folder is unknown", ErrNoAgent)
			}
			if _, err := env.LookPath(env.SystemDir + `\WindowsPowerShell\v1.0\powershell.exe`); err != nil {
				return fmt.Errorf("%w: the Windows script host is not installed", ErrNoAgent)
			}
			return nil
		},
		run: func(ctx context.Context, p InstallPlan) error {
			if !p.Windows {
				return errors.New("this plan is not a Windows plan")
			}
			if env.SystemDir == "" {
				return fmt.Errorf("%w: the system folder is unknown", ErrNoAgent)
			}
			ps := env.SystemDir + `\WindowsPowerShell\v1.0\powershell.exe`
			if _, err := env.LookPath(ps); err != nil {
				return fmt.Errorf("%w: the Windows script host is not installed", ErrNoAgent)
			}
			// A folder under the user's temporary folder: private by Windows' defaults, but the user's own processes can write
			// to it too. What is in it is not trusted for being there: both files are checked against hashes that travel in the
			// elevated process's own command line and script.
			dir, err := env.TempDir()
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			planPath := filepath.Join(dir, "plan.json")
			if err := os.WriteFile(planPath, []byte(p.FullScript()), 0o600); err != nil {
				return err
			}
			inner, err := uacInner(planPath, PlanSHA256(p))
			if err != nil {
				return err
			}
			scriptPath := filepath.Join(dir, "install.ps1")
			if err := os.WriteFile(scriptPath, []byte(inner), 0o600); err != nil {
				return err
			}
			sum := sha256.Sum256([]byte(inner))
			boot, err := uacBoot(scriptPath, hex.EncodeToString(sum[:]))
			if err != nil {
				return err
			}
			launch, err := uacLaunch(ps, boot)
			if err != nil {
				return err
			}
			out, err := env.Exec(ctx, minimalEnv(env.SystemDir, env.Getenv), ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", psEncoded(launch))
			return uacResult(out, err)
		},
	}
}

// uacResult maps the exit status of the launching script.
func uacResult(out []byte, err error) error {
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch ee.ExitCode() {
		case uacDeclined:
			return ErrAdminCancelled
		case uacPlanChanged:
			return &coded{CodePlanChanged, errors.New("the plan or the script changed after it was approved; nothing was installed")}
		case uacFileChanged, uacPluginFiles:
			return &coded{CodeVerify, errors.New("a file was not the one approved; nothing was installed")}
		case uacNotRubi:
			return &coded{CodeBadPath, errors.New("the target is not a Rubi file or folder; nothing was replaced")}
		case uacBusy:
			return &coded{CodeBusy, errors.New("another installation is running")}
		case uacInUse:
			return &coded{CodeInUse, errors.New("the hook is running and could not be replaced")}
		}
	}
	return errors.New("the elevated installer failed: " + trimOut(out, err))
}
