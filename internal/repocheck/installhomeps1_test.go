package repocheck

// panel/install-home.ps1, the installer the Bot runs on a Windows computer (docs/plans/home-platforms.md, stage P11). It cannot
// be run on a Mac or Linux box, so these tests read it: the order of its steps, where it takes OpenSSL from, that the key is
// the release key, that nothing a person writes can enter it. They also run the one command of it that matters most, the
// OpenSSL signature check, with the very arguments the script uses, against a throwaway key (skipped without an OpenSSL that has
// Ed25519). A real run, with a bad signature, a bad checksum and a fake OpenSSL on PATH, is the CI job home-windows-install.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func ps1(t *testing.T) string { return readRepo(t, "panel/install-home.ps1") }

// psCode drops the comment lines of a PowerShell script.
func psCode(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func order(t *testing.T, s string, steps ...string) {
	t.Helper()
	last := -1
	for _, st := range steps {
		i := strings.Index(s, st)
		if i < 0 {
			t.Errorf("install-home.ps1 lacks %q", st)
			return
		}
		if i < last {
			t.Errorf("install-home.ps1: %q comes too early", st)
		}
		last = i
	}
}

// Nothing is unpacked, installed or started before the signature and then the checksum have been checked; pairing is last.
func TestInstallHomePs1ChecksBeforeItInstallsAndPairsLast(t *testing.T) {
	s := psCode(ps1(t))
	main := s[strings.Index(s, "function Main {"):]
	order(t, main,
		"$openssl = Find-GitOpenSsl", // looked for before anything is downloaded
		"Get-Bytes ($release + '/SHA256SUMS')",
		"if (-not (Test-Signature $openssl",
		"ComputeHash($zipBytes)",
		"New-Object System.IO.Compression.ZipArchive",
		"$from.CopyTo($to)",
		"Stop-RunningCopy $bin",
		"[System.IO.File]::Move($new, $bin)",
		"& $bin install-autostart --bin $bin",
		"& $bin open")
	if !strings.Contains(s, `Fail "the release signature doesn't verify; not installing"`) || !strings.Contains(s, "'; not installing'") {
		t.Error("a bad signature or checksum does not stop the install with a message")
	}
	if !strings.Contains(s, "if (-not $NoPairing)") {
		t.Error("pairing is not the last, skippable step")
	}
	// the one file taken from the archive is named by the script, never by a path in the archive
	if !strings.Contains(s, "$e.FullName -ceq 'rubi-home.exe'") || strings.Contains(s, "Expand-Archive") || strings.Contains(s, "ExtractToDirectory") {
		t.Error("the archive is unpacked by a path from the archive")
	}
	// it stops on any error
	if !strings.Contains(s, "$ErrorActionPreference = 'Stop'") || !strings.Contains(s, "Set-StrictMode -Version 3.0") {
		t.Error("the script does not stop on errors")
	}
}

// The bytes that are checked are the bytes that are used: the archive is downloaded into memory, hashed and unpacked from the
// same bytes; the signed list is written once, opened again read-only with writers denied and compared with what was written,
// kept open while OpenSSL reads it, and the checksum line is parsed from the same bytes. No zip file lands on disk to swap.
func TestInstallHomePs1UsesTheBytesItChecked(t *testing.T) {
	s := psCode(ps1(t))
	for _, want := range []string{
		"[System.IO.FileAccess]::Read, [System.IO.FileShare]::Read)", // held: read-only, writers and deleters denied
		"a downloaded file changed while it was being written",
		"$held += $sumsHeld", "$held += $sigHeld", "$held += $keyHeld",
		"foreach ($h in $held) { $h.Dispose() }",             // released only at the end
		"[System.Text.Encoding]::UTF8.GetString($sumsBytes)", // parsed from the same bytes
		"ComputeHash($zipBytes)",
		"New-Object System.IO.MemoryStream(, $zipBytes)", // unpacked from the same bytes
	} {
		if !strings.Contains(s, want) {
			t.Errorf("install-home.ps1 lacks %q", want)
		}
	}
	if strings.Contains(s, "$zip = ") || strings.Contains(s, "WriteAllBytes($zip") || strings.Contains(s, "-OutFile") {
		t.Error("the archive is written to a file before it is checked")
	}
	if i, j := strings.Index(s, "Open-Held ([System.IO.Path]::Combine($tmp, 'SHA256SUMS'))"), strings.Index(s, "if (-not (Test-Signature"); i < 0 || j < i {
		t.Error("the signed list is not held open before OpenSSL reads it")
	}
}

// An upgrade does not leave the computer without a working Rubi Home: the old file is put back when the new cannot be moved
// in, a running copy is ended first, and files that could not be removed are named.
func TestInstallHomePs1UpgradeIsSafe(t *testing.T) {
	s := psCode(ps1(t))
	for _, want := range []string{
		"[System.IO.File]::Move($bin, $old)",
		"[System.IO.File]::Move($old, $bin)", // restored
		"the old one was left as it was",
		"function Stop-RunningCopy", "GetProcessesByName('rubi-home')", "MainModule.FileName -ieq $Exe",
		"rubi-home.exe.old-*", "an old copy is still in use and was left: ",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("install-home.ps1 lacks %q", want)
		}
	}
	i, j, k := strings.Index(s, "Stop-RunningCopy $bin"), strings.Index(s, "[System.IO.File]::Move($bin, $old)"), strings.Index(s, "& $bin install-autostart")
	if i < 0 || j < i || k < j {
		t.Error("the running copy must be ended, then the old file moved aside, then the autostart set up")
	}
}

// The key in the script is the release key the rest of Rubi embeds (release-key.pub.pem).
func TestInstallHomePs1CarriesTheReleaseKey(t *testing.T) {
	pem := strings.TrimSpace(readRepo(t, "release-key.pub.pem"))
	m := regexp.MustCompile(`(?s)\$ReleaseKey = @'\r?\n(.*?)\r?\n'@`).FindStringSubmatch(ps1(t))
	if m == nil {
		t.Fatal("no $ReleaseKey here-string")
	}
	if strings.TrimSpace(m[1]) != pem {
		t.Errorf("the key in install-home.ps1 is not release-key.pub.pem:\n%s", m[1])
	}
	sh := readRepo(t, "panel/install-home.sh")
	body := strings.Join(strings.Fields(strings.SplitN(strings.SplitN(pem, "-----BEGIN PUBLIC KEY-----", 2)[1], "-----END", 2)[0]), "")
	if !strings.Contains(sh, body) {
		t.Error("install-home.sh and install-home.ps1 do not carry the same key")
	}
}

// OpenSSL comes from Git for Windows' own folder and nowhere else: no PATH, no search by name, only admin-owned files, its
// own libraries too, and the folders above; it runs with a clean environment in its own folder and must say it verified.
func TestInstallHomePs1TakesOpenSslOnlyFromGitsAdminFolder(t *testing.T) {
	s := psCode(ps1(t))
	for _, bad := range []string{"Get-Command", "where.exe", "$env:PATH", "$env:Path", "Resolve-Path", "Start-Process", "& openssl", "'openssl'", "\"openssl\"", "Invoke-Expression", "iex ", "| iex"} {
		if strings.Contains(s, bad) {
			t.Errorf("install-home.ps1 contains %q", bad)
		}
	}
	// the only places OpenSSL is run from are $Exe / $openssl, the result of Find-GitOpenSsl
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(strings.ToLower(l), "openssl") && !strings.Contains(l, "$openssl") && !strings.Contains(l, "Find-GitOpenSsl") &&
			!strings.Contains(l, "openssl.exe") && !strings.Contains(l, "OpenSSL") && !strings.Contains(l, "OPENSSL") && !strings.Contains(l, "ask") {
			t.Errorf("a line names openssl in an unexpected way: %q", l)
		}
	}
	if !strings.Contains(s, "$openssl = Find-GitOpenSsl") || !strings.Contains(s, "if (-not $openssl)") {
		t.Error("a missing OpenSSL is not refused")
	}
	for _, want := range []string{
		"'SOFTWARE\\GitForWindows'", "Registry64", "Registry32", "GetFolderPath('ProgramFiles')", "$env:ProgramW6432", // Git's folder
		"Test-AdminOnly $path $false", "Test-AdminOnly $dll $false", "Test-AdminOnly $above $true", // the file, its libraries, the folders above
		"GetFiles([System.IO.Path]::GetDirectoryName($exe), '*.dll')",
		"S-1-5-32-544", "S-1-5-18", // Administrators, SYSTEM
		"ReparsePoint", "GetOwner(", "GetAccessRules(",
		"Git for Windows is needed", "is not used", // the message for the Bot
		// the run: a clean environment, its own folder, the success line
		"$name -like 'OPENSSL*'", "$psi.EnvironmentVariables.Remove($name)", "$psi.WorkingDirectory = $dir", "Signature Verified Successfully",
		"$p.ExitCode -eq 0",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("install-home.ps1 lacks %q", want)
		}
	}
	if strings.Contains(s, "WorkingDirectory = $tmp") || strings.Contains(s, "Set-Location") || strings.Contains(s, "Push-Location") {
		t.Error("OpenSSL runs in the download folder or the script changes folder")
	}
	if !strings.Contains(s, "continue") || !strings.Contains(s, "return ''") {
		t.Error("Find-GitOpenSsl has no way to find nothing")
	}
}

// Nothing that PowerShell would load from a folder the user can write: no cmdlet of the modules it autoloads (the user's
// module folder comes first on PSModulePath), .NET instead.
func TestInstallHomePs1UsesNoAutoloadedCmdlets(t *testing.T) {
	s := psCode(ps1(t))
	for _, bad := range []string{"Get-Acl", "Set-Acl", "Get-FileHash", "Get-Item", "Get-ItemProperty", "Test-Path", "Invoke-WebRequest", "Invoke-RestMethod",
		"Add-Type", "New-Item", "Move-Item", "Remove-Item", "Copy-Item", "Get-ChildItem", "Join-Path", "Split-Path", "Expand-Archive", "ConvertFrom-Json",
		"Set-Content", "Get-Content", "Out-File", "Import-Module", "Get-Module"} {
		if strings.Contains(s, bad) {
			t.Errorf("install-home.ps1 uses %s", bad)
		}
	}
}

// No text from anyone is put into the script or run as code: the tag is checked by a pattern before it is used, the
// environment cannot change an address or a folder unless the test flag is set (and then only to this computer), and nothing
// is evaluated. Redirects away from https are refused.
func TestInstallHomePs1IsConstant(t *testing.T) {
	s := psCode(ps1(t))
	if !strings.Contains(s, `if ($Version -cnotmatch '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?\z') { Fail 'bad version' }`) {
		t.Error("the release tag is not checked by an anchored pattern (\\z, not $)")
	}
	pat := strings.Index(s, "-cnotmatch")
	use := strings.Index(s, "$archive = ")
	if pat < 0 || use < pat {
		t.Error("the tag is used before it is checked")
	}
	for _, bad := range []string{"Invoke-Expression", "iex", "Add-Type -TypeDefinition", "-EncodedCommand", "[scriptblock]::Create", "Start-Process", "cmd /c", "cmd.exe"} {
		if strings.Contains(s, bad) {
			t.Errorf("install-home.ps1 contains %q", bad)
		}
	}
	// the environment: only fixed, harmless names are read directly; the four overrides only through Get-TestSetting
	env := regexp.MustCompile(`\$env:([A-Z_0-9a-z]+)`).FindAllStringSubmatch(s, -1)
	allowed := map[string]bool{"OS": true, "PROCESSOR_ARCHITEW6432": true, "PROCESSOR_ARCHITECTURE": true, "RUBI_INSTALL_TEST": true, "ProgramW6432": true}
	for _, m := range env {
		if !allowed[m[1]] {
			t.Errorf("install-home.ps1 reads $env:%s directly", m[1])
		}
	}
	if !strings.Contains(s, "$TestMode = ($env:RUBI_INSTALL_TEST -eq '1')") {
		t.Error("no test flag")
	}
	fn := s[strings.Index(s, "function Get-TestSetting"):]
	fn = fn[:strings.Index(fn, "\n}\n")]
	if i, j := strings.Index(fn, "if (-not $TestMode) { return '' }"), strings.Index(fn, "GetEnvironmentVariable"); i < 0 || j < i {
		t.Errorf("Get-TestSetting reads the environment before checking the flag:\n%s", fn)
	}
	if n := strings.Count(s, "GetEnvironmentVariable"); n != 1 {
		t.Errorf("the environment is read in %d places besides Get-TestSetting", n-1)
	}
	for _, name := range []string{"RUBI_RELEASE_FEED", "RUBI_DOWNLOAD_BASE", "RUBI_HOME_INSTALL", "RUBI_GIT_ROOT"} {
		if !strings.Contains(s, "Get-TestSetting '"+name+"'") {
			t.Errorf("%s is not read through Get-TestSetting", name)
		}
	}
	if !strings.Contains(s, "in test mode the download address must be this computer") || !strings.Contains(s, "Test-Loopback $uri") {
		t.Error("a test address is not required to be this computer")
	}
	// https only, and a redirect from https to anything else is refused
	for _, want := range []string{"$Uri.Scheme -eq 'https'", "$allowHttp = ($TestMode -and (Test-Loopback $current))", "$req.AllowAutoRedirect = $false",
		"if ($current.Scheme -eq 'https' -and $next.Scheme -ne 'https') { Fail ('refusing a redirect from https to ' + $next.Scheme) }"} {
		if !strings.Contains(s, want) {
			t.Errorf("install-home.ps1 lacks %q", want)
		}
	}
	if !strings.Contains(s, "$feed = 'https://rubi-panel.com/releases/latest.json'") || !strings.Contains(s, "$base = 'https://github.com/' + $Repo + '/releases/download'") {
		t.Error("the default addresses changed")
	}
	// the files are named by the release's tag and the CPU, the way release.yml names them
	if !strings.Contains(s, "'rubi-home-' + $Version + '-windows-' + $arch + '.zip'") {
		t.Error("the archive name is not the one release.yml builds")
	}
	if !regexp.MustCompile(`(?s)param\(\s*\[string\]\$Version = '',\s*\[switch\]\$NoPairing\s*\)`).MatchString(s) {
		t.Error("the script takes other parameters than the optional tag and -NoPairing")
	}
	// the call is last: a download cut short runs nothing
	if !strings.HasSuffix(strings.TrimSpace(s), "}") || !strings.Contains(s, "try {\n  Main\n} catch {") {
		t.Error("Main is not called last, inside the error handler")
	}
}

// A syntax check without PowerShell: every brace, bracket and parenthesis closes, here-strings end, quotes pair up per line
// outside here-strings and comments. (Run `pwsh -NoProfile -Command "[scriptblock]::Create((Get-Content -Raw panel/install-home.ps1))"` where pwsh
// exists; the Windows job runs the script itself.)
func TestInstallHomePs1IsBalanced(t *testing.T) {
	raw := ps1(t)
	if strings.Contains(raw, "\r") {
		t.Error("the script has CR characters; keep it with LF line endings")
	}
	// here-strings
	if n := strings.Count(raw, "@'\n"); n != strings.Count(raw, "\n'@") {
		t.Errorf("%d here-strings open, %d close", n, strings.Count(raw, "\n'@"))
	}
	body := regexp.MustCompile(`(?s)@'\n.*?\n'@`).ReplaceAllString(raw, "''")
	var depth = map[rune]int{}
	pairs := map[rune]rune{'}': '{', ')': '(', ']': '['}
	for i, line := range strings.Split(body, "\n") {
		inS, inD := false, false
		for _, r := range line {
			switch {
			case inS:
				if r == '\'' {
					inS = false
				}
			case inD:
				if r == '"' {
					inD = false
				}
			case r == '#':
				goto next
			case r == '\'':
				inS = true
			case r == '"':
				inD = true
			case r == '{' || r == '(' || r == '[':
				depth[r]++
			case r == '}' || r == ')' || r == ']':
				depth[pairs[r]]--
				if depth[pairs[r]] < 0 {
					t.Fatalf("line %d: %q closes what was never opened", i+1, string(r))
				}
			}
		}
		if inS || inD {
			t.Errorf("line %d: a quote is not closed: %s", i+1, line)
		}
	next:
	}
	for r, n := range depth {
		if n != 0 {
			t.Errorf("%q is not closed (%d open)", string(r), n)
		}
	}
}

// If PowerShell is here, it parses the script (the parser only; nothing runs).
func TestInstallHomePs1ParsesWithPowerShell(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh is not installed")
	}
	cmd := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command",
		"$e = $null; [void][System.Management.Automation.Language.Parser]::ParseFile($args[0], [ref]$null, [ref]$e); if ($e) { $e | Out-String; exit 1 }",
		filepath.Join(root(t), "panel", "install-home.ps1"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// The signature check with the script's own arguments: a good signature verifies and a bad one is refused (exit status), and
// the checksum list's line pattern finds the archive in the real SHA256SUMS format.
func TestInstallHomePs1SignatureCheckRefusesABadSignature(t *testing.T) {
	var openssl string
	for _, c := range []string{"openssl", "/opt/homebrew/bin/openssl", "/usr/local/opt/openssl/bin/openssl"} {
		p, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		if exec.Command(p, "genpkey", "-algorithm", "ed25519", "-out", filepath.Join(t.TempDir(), "k.pem")).Run() == nil {
			openssl = p
			break
		}
	}
	if openssl == "" {
		t.Skip("no OpenSSL with Ed25519")
	}
	call := regexp.MustCompile(`\$psi\.Arguments = \('(pkeyutl -verify[^']*)' -f`).FindStringSubmatch(ps1(t))
	if call == nil {
		t.Fatal("no OpenSSL verify call in the script")
	}
	dir := t.TempDir()
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(openssl, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
	}
	run("key", "genpkey", "-algorithm", "ed25519", "-out", filepath.Join(dir, "key.pem"))
	run("pub", "pkey", "-in", filepath.Join(dir, "key.pem"), "-pubout", "-out", filepath.Join(dir, "release-key.pem"))
	sums := filepath.Join(dir, "SHA256SUMS")
	os.WriteFile(sums, []byte(strings.Repeat("a", 64)+"  rubi-home-v1.0.0-windows-amd64.zip\n"), 0o644)
	run("sign", "pkeyutl", "-sign", "-inkey", filepath.Join(dir, "key.pem"), "-rawin", "-in", sums, "-out", filepath.Join(dir, "SHA256SUMS.sig"))
	verify := func() error {
		line := strings.NewReplacer(`"{0}"`, `"`+filepath.Join(dir, "release-key.pem")+`"`, `"{1}"`, `"`+sums+`"`, `"{2}"`, `"`+filepath.Join(dir, "SHA256SUMS.sig")+`"`).Replace(call[1])
		args := strings.Fields(strings.ReplaceAll(line, `"`, ""))
		out, err := exec.Command(openssl, args...).CombinedOutput()
		if err == nil && !strings.Contains(string(out), "Signature Verified Successfully") {
			t.Errorf("a good check did not print the line the script requires: %q", out)
		}
		return err
	}
	if err := verify(); err != nil {
		t.Fatalf("a good signature was refused with the script's arguments: %v", err)
	}
	// a changed list
	os.WriteFile(sums, []byte(strings.Repeat("b", 64)+"  rubi-home-v1.0.0-windows-amd64.zip\n"), 0o644)
	if err := verify(); err == nil {
		t.Error("a signature of another list was accepted")
	}
	// a changed signature
	os.WriteFile(sums, []byte(strings.Repeat("a", 64)+"  rubi-home-v1.0.0-windows-amd64.zip\n"), 0o644)
	sigb, _ := os.ReadFile(filepath.Join(dir, "SHA256SUMS.sig"))
	sigb[0] ^= 1
	os.WriteFile(filepath.Join(dir, "SHA256SUMS.sig"), sigb, 0o644)
	if err := verify(); err == nil {
		t.Error("a damaged signature was accepted")
	}
	// the line pattern of the script, in this syntax, finds exactly the archive
	pat := regexp.MustCompile("\\[regex\\]::Match\\(\\$line\\.TrimEnd\\(\"`r\"\\), '([^']+)'\\)").FindStringSubmatch(ps1(t))
	if pat == nil {
		t.Fatal("no checksum line pattern")
	}
	re := regexp.MustCompile(pat[1])
	h := strings.Repeat("c", 64)
	for line, want := range map[string]string{
		h + "  rubi-home-v1.0.0-windows-amd64.zip":  "rubi-home-v1.0.0-windows-amd64.zip",
		h + " *rubi-home-v1.0.0-windows-amd64.zip":  "rubi-home-v1.0.0-windows-amd64.zip",
		h + "  bin/rubi-linux-amd64":                "bin/rubi-linux-amd64",
		h + "  two words.zip":                       "",
		"short  rubi-home-v1.0.0-windows-amd64.zip": "",
	} {
		m := re.FindStringSubmatch(line)
		got := ""
		if m != nil {
			got = m[2]
		}
		if got != want {
			t.Errorf("%q: %q, want %q", line, got, want)
		}
	}
}

// The release builds what the script downloads: rubi-home-<tag>-windows-<arch>.zip holding rubi-home.exe, tested on Windows
// before it is released, and listed in the signed sums.
func TestReleaseCarriesTheWindowsZips(t *testing.T) {
	s := readRepo(t, ".github/workflows/release.yml")
	job := strings.Index(s, "\n  windows-home:\n")
	chk := strings.Index(s, "\n  windows-check:\n")
	rel := strings.Index(s, "\n  release:\n")
	if job < 0 || chk < job || rel < chk {
		t.Fatal("release.yml needs the jobs windows-home, windows-check and release, in that order")
	}
	build, check := s[job:chk], s[chk:rel]
	for _, want := range []string{"for arch in amd64 arm64; do", "GOOS=windows GOARCH=$arch", "-o \"$out/rubi-home.exe\"",
		`rubi-home-${GITHUB_REF_NAME}-windows-$arch.zip`, "rubi-home.exe)"} {
		if !strings.Contains(build, want) {
			t.Errorf("the windows-home job lacks %q", want)
		}
	}
	if !strings.Contains(check, "runs-on: windows-latest") || !strings.Contains(check, "rubi-home.exe version") {
		t.Error("the Windows zip is not run on Windows before the release")
	}
	if !strings.Contains(s[rel:rel+200], "needs: [mac-app, windows-check]") {
		t.Error("the release does not wait for the Windows check")
	}
	dl := strings.Index(s[rel:], "name: windows-home")
	sums := strings.Index(s, "sha256sum rubi-*.tar.gz rubi-*.zip")
	if dl < 0 || sums < 0 || rel+dl > sums {
		t.Error("the Windows zips are not downloaded into dist before SHA256SUMS lists them")
	}
	if !strings.Contains(s, `dist/rubi-home-"${GITHUB_REF_NAME}"-windows-amd64.zip dist/rubi-home-"${GITHUB_REF_NAME}"-windows-arm64.zip`) {
		t.Error("the release does not check that both Windows zips arrived")
	}
	// each zip is looked at on Windows: one entry named rubi-home.exe, and a program for the zip's CPU
	for _, want := range []string{"Each zip holds the one program, for its CPU", "0x8664", "0xAA64", "-cne 'rubi-home.exe'"} {
		if !strings.Contains(check, want) {
			t.Errorf("the Windows check lacks %q", want)
		}
	}
	// the name the script looks for
	if !strings.Contains(psCode(ps1(t)), "'-windows-'") {
		t.Error("the installer does not look for the windows zip by that name")
	}
}

// The Windows CI job runs the script for real and tries the three refusals.
func TestCIRunsTheWindowsInstallerAgainstAFakeRelease(t *testing.T) {
	s := readRepo(t, ".github/workflows/ci.yml")
	i := strings.Index(s, "\n  home-windows-install:\n")
	if i < 0 {
		t.Fatal("no home-windows-install job")
	}
	job := s[i:]
	if j := strings.Index(job[3:], "\n  home-windows:\n"); j >= 0 {
		job = job[:j+3]
	}
	for _, want := range []string{"runs-on: windows-latest", "install-home.ps1", "RUBI_DOWNLOAD_BASE", "-NoPairing", "a bad signature was not refused",
		"a bad checksum was not refused", "an openssl on PATH was used", "schtasks /Query", "install-autostart --remove", "genpkey -algorithm ed25519",
		"an OpenSSL in a user-writable folder was used", "RUBI_GIT_ROOT", "the environment changed where the installer downloads from or installs to",
		"a test address that is not this computer was accepted", "$env:OPENSSL_CONF = $conf", "openssl_conf = nothing_here", "RUBI_INSTALL_TEST"} {
		if !strings.Contains(job, want) {
			t.Errorf("the job lacks %q", want)
		}
	}
}

// The Windows part of install.md tells the Bot what to run and what the user sees, with no command for the user.
func TestInstallMdHasAWindowsSection(t *testing.T) {
	skill := readRepo(t, "skills/grok-bot/rubi-project/SKILL.md")
	i := strings.Index(skill, "### Windows")
	if i < 0 {
		t.Fatal("no Windows subsection")
	}
	sec := skill[i:]
	if j := strings.Index(sec[3:], "\n## "); j >= 0 {
		sec = sec[:j+3]
	}
	for _, want := range []string{"https://rubi-panel.com/install-home.ps1", "-ExecutionPolicy Bypass -File", "-UseBasicParsing", "Tls12", "NewGuid()", "Remove-Item -LiteralPath $u",
		"trusted because it came over TLS from rubi-panel.com", "**Git for Windows.**", "winget install --id Git.Git -e",
		"Nothing to confirm for Rubi Home itself", "Open the Rubi panel", "allow this app to make changes", "unknown", "Scheduled Task", "not signed"} {
		if !strings.Contains(sec, want) {
			t.Errorf("the Windows subsection lacks %q", want)
		}
	}
	for _, bad := range []string{"sudo", "RunAs", "Start-Process -Verb"} {
		if strings.Contains(strings.ReplaceAll(sec, "Don't open an elevated shell", ""), bad) {
			t.Errorf("the Windows subsection tells the Bot to use %q", bad)
		}
	}
}
