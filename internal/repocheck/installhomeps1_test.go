package repocheck

// panel/install-home.ps1, the installer the Bot runs on a Windows computer (docs/plans/home-platforms.md, stage P11). It cannot
// be run on a Mac or Linux box, so these tests read it: the order of its steps, that the signature is checked by code inside
// the script on the bytes that are then used, that the key is the release key, that nothing a person writes can enter it. The
// Ed25519 check itself is tested on test vectors: here against Go's crypto/ed25519 (the vectors file), and by the script
// scripts/test-install-home-ed25519.ps1 where PowerShell exists (CI runs it under Windows PowerShell 5.1 and PowerShell 7). A
// real run, with a bad signature, a malleated one and a bad checksum, is the CI job home-windows-install.

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
		"Get-Bytes ($release + '/SHA256SUMS')",
		"if (-not (Test-Ed25519 (ConvertFrom-Ed25519Pem $ReleaseKey) $sumsBytes $sigBytes))",
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

// The bytes that are checked are the bytes that are used: everything is downloaded into memory, the signature is checked on the
// signed list's bytes, the checksum line is parsed from those same bytes, and the archive is hashed and unpacked from the same
// bytes. Nothing is written to disk before the check, so there is no file to swap.
func TestInstallHomePs1UsesTheBytesItChecked(t *testing.T) {
	s := psCode(ps1(t))
	main := s[strings.Index(s, "function Main {"):]
	for _, want := range []string{
		"Test-Ed25519 (ConvertFrom-Ed25519Pem $ReleaseKey) $sumsBytes $sigBytes", // the signature, on the list's bytes
		"[System.Text.Encoding]::UTF8.GetString($sumsBytes)",                     // parsed from the same bytes
		"ComputeHash($zipBytes)",
		"New-Object System.IO.MemoryStream(, $zipBytes)", // unpacked from the same bytes
	} {
		if !strings.Contains(main, want) {
			t.Errorf("install-home.ps1 lacks %q", want)
		}
	}
	check := strings.Index(main, "Test-Ed25519 (")
	if check < 0 {
		t.Fatal("no signature check in Main")
	}
	for _, bad := range []string{"WriteAllBytes", "-OutFile", "GetTempPath", "GetTempFileName", "$tmp", "$zip = ", "File]::Create", "Set-Content", "Out-File"} {
		if strings.Contains(s, bad) {
			t.Errorf("install-home.ps1 contains %q: something may be written before the check", bad)
		}
	}
	// The signature that matters here is the list's (the feed has its own check, earlier); the installed copy is only asked
	// for its version before it, and nothing on disk is made, moved or deleted.
	check = strings.Index(main, "Test-Ed25519 (ConvertFrom-Ed25519Pem $ReleaseKey) $sumsBytes $sigBytes")
	if check < 0 {
		t.Fatal("no check of the signed list in Main")
	}
	for _, act := range []string{"CreateDirectory", "File]::Move", "File]::Delete", "Stop-RunningCopy $bin", "FileStream"} {
		if i := strings.Index(main, act); i >= 0 && i < check {
			t.Errorf("%q comes before the signed list is checked", act)
		}
	}
	if i := strings.Index(main, "ZipArchive"); i < check {
		t.Error("the archive is opened before the signature is checked")
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

// No program is run to check the signature: the check is code in the script (one region), not OpenSSL, not Git, not any
// process. Its strict rules are in the code, and System.Numerics is loaded by its full assembly name (no Add-Type).
func TestInstallHomePs1RunsNoProgramToCheckTheSignature(t *testing.T) {
	raw := ps1(t)
	s := psCode(raw)
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(strings.ToLower(l), "openssl") {
			t.Errorf("a code line names openssl: %q", l)
		}
	}
	// the one ProcessStartInfo is the installed copy's `version`, read with a time limit, outside the check
	if fn := s[strings.Index(s, "function Get-InstalledVersion("):]; strings.Count(s, "ProcessStartInfo") != 1 ||
		!strings.Contains(fn[:strings.Index(fn, "\n}\n")], "New-Object System.Diagnostics.ProcessStartInfo") ||
		!strings.Contains(fn[:strings.Index(fn, "\n}\n")], "$p.WaitForExit(5000)") {
		t.Error("a process is started outside Get-InstalledVersion, or without its time limit")
	}
	s = strings.Replace(s, "New-Object System.Diagnostics.ProcessStartInfo", "", 1)
	for _, bad := range []string{"ProcessStartInfo", "GitForWindows", "RUBI_GIT_ROOT", "Find-Git", "Get-Command", "where.exe", "$env:PATH", "$env:Path", "Start-Process"} {
		if strings.Contains(s, bad) {
			t.Errorf("install-home.ps1 contains %q", bad)
		}
	}
	if strings.Count(raw, "#region ed25519") != 1 || strings.Count(raw, "#endregion ed25519") != 1 {
		t.Fatal("the script needs exactly one #region ed25519 ... #endregion ed25519")
	}
	region := raw[strings.Index(raw, "#region ed25519"):strings.Index(raw, "#endregion ed25519")]
	if strings.Count(raw, "function Test-Ed25519(") != 1 || !strings.Contains(region, "function Test-Ed25519(") {
		t.Error("Test-Ed25519 is not defined once, inside the region")
	}
	for _, want := range []string{
		"if ($s -ge $EdL) { return $false }",                                                 // S below L
		"if ($y -ge $p) { return $null }",                                                    // a canonical y
		"if ($x.IsZero -and $sign -eq 1) { return $null }",                                   // no negative zero
		"if ($enc[$i] -ne $r[$i]) { $same = $false }",                                        // the result is compared with R, byte by byte
		"System.Numerics, Version=4.0.0.0, Culture=neutral, PublicKeyToken=b77a5c561934e089", // loaded by full name
	} {
		if !strings.Contains(region, want) {
			t.Errorf("the ed25519 region lacks %q", want)
		}
	}
	if !strings.Contains(region, "[System.Reflection.Assembly]::Load(") || strings.Contains(s, "Add-Type") {
		t.Error("System.Numerics is not loaded by assembly name, or Add-Type is used")
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
	// the environment: only fixed, harmless names are read directly; the three overrides only through Get-TestSetting
	env := regexp.MustCompile(`\$env:([A-Z_0-9a-z]+)`).FindAllStringSubmatch(s, -1)
	allowed := map[string]bool{"OS": true, "PROCESSOR_ARCHITEW6432": true, "PROCESSOR_ARCHITECTURE": true, "RUBI_INSTALL_TEST": true}
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
	for _, name := range []string{"RUBI_RELEASE_FEED", "RUBI_DOWNLOAD_BASE", "RUBI_HOME_INSTALL"} {
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
		"$e = $null; [void][System.Management.Automation.Language.Parser]::ParseFile($env:RUBI_PS1_TO_PARSE, [ref]$null, [ref]$e); if ($e) { $e | Out-String; exit 1 }")
	cmd.Env = append(os.Environ(), "RUBI_PS1_TO_PARSE="+filepath.Join(root(t), "panel", "install-home.ps1")) // -Command does not bind $args
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// The checksum list's line pattern finds the archive in the real SHA256SUMS format, and nothing else.
func TestInstallHomePs1ChecksumLinePattern(t *testing.T) {
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
	if !strings.Contains(s[rel:rel+200], "needs: [mac-app, windows-check, microvm]") {
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

// The Windows CI job runs the script for real and tries the refusals; the vectors run under Windows PowerShell 5.1 and on
// PowerShell 7.
func TestCIRunsTheWindowsInstallerAgainstAFakeRelease(t *testing.T) {
	s := readRepo(t, ".github/workflows/ci.yml")
	i := strings.Index(s, "\n  home-windows-install:\n")
	if i < 0 {
		t.Fatal("no home-windows-install job")
	}
	job := s[i+3:]
	if j := strings.Index(job, "\n  # "); j >= 0 {
		job = job[:j]
	}
	for _, want := range []string{"runs-on: windows-latest", "install-home.ps1", "RUBI_DOWNLOAD_BASE", "-NoPairing", "a bad signature was not refused",
		"a malleated signature (S + L) was not refused", "$big + $order", "a bad checksum was not refused", "schtasks /Query", "install-autostart --remove",
		"genpkey -algorithm ed25519", "the environment changed where the installer downloads from or installs to",
		"a test address that is not this computer was accepted", "RUBI_INSTALL_TEST",
		"a feed without a signature was not refused", "a tampered feed was not refused", "a signed list of another release was not refused",
		"an older release was installed over a newer one", "a replayed feed installed an older release", "RUBI_RELEASE_FEED", "latest.json.sig",
		"release feed is not signed", "the signed checksums are for", "older than the installed", "the upgrade through the feed failed"} {
		if !strings.Contains(job, want) {
			t.Errorf("the job lacks %q", want)
		}
	}
	for _, gone := range []string{"RUBI_GIT_ROOT", "OPENSSL_CONF", "an openssl on PATH"} {
		if strings.Contains(job, gone) {
			t.Errorf("the job still has %q, which only made sense for an installer that ran OpenSSL", gone)
		}
	}
	// the vector script: Windows PowerShell 5.1 on Windows, PowerShell 7 on Linux
	win := s[strings.Index(s, "\n  home-ed25519-windows:\n"):]
	win = win[:strings.Index(win[3:], "\n  home-ed25519-pwsh:\n")+3]
	if !strings.Contains(win, "runs-on: windows-latest") || !strings.Contains(win, "shell: powershell") || !strings.Contains(win, "scripts/test-install-home-ed25519.ps1") {
		t.Error("the vectors do not run under Windows PowerShell 5.1 on windows-latest")
	}
	lin := s[strings.Index(s, "\n  home-ed25519-pwsh:\n"):]
	lin = lin[:strings.Index(lin[3:], "\n  # ")+3]
	if !strings.Contains(lin, "runs-on: ubuntu-latest") || !strings.Contains(lin, "shell: pwsh") || !strings.Contains(lin, "scripts/test-install-home-ed25519.ps1") {
		t.Error("the vectors do not run under pwsh on ubuntu-latest")
	}
	// both also check the newest published release's real signature with the key in the script
	for name, job := range map[string]string{"home-ed25519-windows": win, "home-ed25519-pwsh": lin} {
		for _, want := range []string{"GH_TOKEN: ${{ github.token }}", "releases?per_page=20", "-not $_.draft", "$_.name -eq 'SHA256SUMS.sig'", "Select-Object -Last 1", "--pattern SHA256SUMS --pattern SHA256SUMS.sig",
			"scripts/test-install-home-ed25519.ps1 -Sums real/SHA256SUMS -Sig real/SHA256SUMS.sig"} {
			if !strings.Contains(job, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
}

// The release checks its real signature with the installer's own Ed25519 code before it is published.
func TestReleaseChecksTheSignatureWithTheInstallersCode(t *testing.T) {
	s := readRepo(t, ".github/workflows/release.yml")
	rel := s[strings.Index(s, "\n  release:\n"):]
	step := "pwsh -NoProfile -File scripts/test-install-home-ed25519.ps1 -Sums dist/SHA256SUMS -Sig dist/SHA256SUMS.sig"
	sign := strings.Index(rel, "openssl pkeyutl -verify -pubin -inkey release-key.pub.pem")
	run := strings.Index(rel, step)
	pub := strings.Index(rel, "gh release create")
	if sign < 0 || run < sign || pub < run {
		t.Errorf("the release must verify with openssl (%d), then with the installer's code (%d), then publish (%d)", sign, run, pub)
	}
	if !strings.Contains(rel, "runs-on: ubuntu-latest") {
		t.Error("the release job is not on a runner that has pwsh")
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
		"trusted because it came over TLS from rubi-panel.com", "Nothing to confirm for Rubi Home itself", "Open the Rubi panel", "allow this app to make changes", "unknown", "Scheduled Task", "not signed"} {
		if !strings.Contains(sec, want) {
			t.Errorf("the Windows subsection lacks %q", want)
		}
	}
	flat := strings.Join(strings.Fields(sec), " ")
	for _, want := range []string{"checks the release signature itself", "needs no Git, no OpenSSL and no other program"} {
		if !strings.Contains(flat, want) {
			t.Errorf("the Windows subsection lacks %q", want)
		}
	}
	for _, bad := range []string{"sudo", "RunAs", "Start-Process -Verb", "winget", "Git for Windows"} {
		if strings.Contains(strings.ReplaceAll(sec, "Don't open an elevated shell", ""), bad) {
			t.Errorf("the Windows subsection tells the Bot to use %q", bad)
		}
	}
}

// The release-version rule (docs/design/rubi-home.md) in the script's steps: the feed's signature is checked before its
// version is read; the older-release check comes before anything is made, moved, stopped or replaced; the signed list is
// checked for another release's files before the archive is unpacked.
func TestInstallHomePs1FollowsTheReleaseVersionRule(t *testing.T) {
	s := psCode(ps1(t))
	main := s[strings.Index(s, "function Main {"):]
	order(t, main,
		"Get-Bytes $feed 1048576",
		"Get-Bytes ($feed + '.sig') 4096",
		"Test-Ed25519 (ConvertFrom-Ed25519Pem $ReleaseKey) $feedBytes $feedSig",
		"[System.Text.Encoding]::UTF8.GetString($feedBytes)", // the version is read from the bytes that were verified
		"$Version = $found.Groups[1].Value",
		"Get-InstalledVersion $bin",
		"Test-ReleaseNewer $have $Version",
		"Get-Bytes ($release + '/SHA256SUMS')",
		"Test-Ed25519 (ConvertFrom-Ed25519Pem $ReleaseKey) $sumsBytes $sigBytes",
		"Get-OtherVersion ([System.Text.Encoding]::UTF8.GetString($sumsBytes)) $Version",
		"ComputeHash($zipBytes)",
		"CreateDirectory($bindir)",
		"Stop-RunningCopy $bin",
		"[System.IO.File]::Move($new, $bin)")
	for _, want := range []string{"the release feed is not signed with the Rubi release key", "is older than the installed", "the signed checksums are for"} {
		if !strings.Contains(main, want) {
			t.Errorf("Main lacks the message %q", want)
		}
	}
	// no way around it: no switch, no environment name for it
	for _, bad := range []string{"-Force", "$Force", "AllowDowngrade", "RUBI_ALLOW"} {
		if strings.Contains(s, bad) {
			t.Errorf("install-home.ps1 has %q: the older-release refusal has no override", bad)
		}
	}
	// the version never comes from the feed without its signature: the only read of "version" is after the check
	if strings.Count(main, `"version"`) != 1 {
		t.Error(`the script reads "version" from text more than once`)
	}
}
