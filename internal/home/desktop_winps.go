package home

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// winDesktop is the Windows form of Desktop. There is no shell in it and no `cmd /c`:
//
//   - a link goes to rundll32 as one argument (url.dll,FileProtocolHandler <link>), which hands it to the system's
//     own handler. Go quotes an argument for CreateProcess; nothing parses it afterwards, and checkLink has refused
//     every character that cmd.exe or the quoting could treat specially.
//   - the clipboard is clip.exe with the text on its standard input.
//   - a window is a PowerShell script that is a constant of this file. The text, the title and the buttons are
//     handed over as JSON on standard input (plain ASCII, so no code page can change them) and read with
//     ConvertFrom-Json; the script never contains any of them, and they are never part of a command line.
func winDesktop(env DesktopEnv) Desktop {
	env = env.withDefaults()
	if env.SystemDir == "" {
		return Desktop{}
	}
	// the programs are those of the System32 folder, by full path: a copy earlier in PATH or in the working folder is
	// never run
	rundll32 := env.SystemDir + `\rundll32.exe`
	clip := env.SystemDir + `\clip.exe`
	powershell := env.SystemDir + `\WindowsPowerShell\v1.0\powershell.exe`
	has := func(path string) bool { _, err := env.LookPath(path); return err == nil }
	var d Desktop
	if has(rundll32) {
		d.Opener = openerFunc(func(ctx context.Context, link string) error {
			if err := checkLink(link); err != nil {
				return err
			}
			_, err := env.Run(ctx, "", rundll32, "url.dll,FileProtocolHandler", link)
			return err
		})
	}
	if has(clip) {
		d.Clipboard = clipboardFunc(func(ctx context.Context, text string) error {
			_, err := env.Run(ctx, text, clip)
			return err
		})
	}
	if has(powershell) {
		d.Dialogs = dialogsFunc(func(ctx context.Context, text string, buttons ...string) (string, error) {
			if err := checkButtons(buttons); err != nil {
				return "", err
			}
			in, err := asciiJSON(map[string]any{"title": dialogTitle, "text": text, "buttons": buttons})
			if err != nil {
				return "", err
			}
			out, err := env.Run(ctx, in, powershell, psArgs...)
			if err != nil {
				// Only a session that can show no window (the script exits 3) or a PowerShell that is not there is "no
				// desktop"; any other failure is an error, and the pairing code is not printed because of it.
				var ee *exec.ExitError
				if errors.Is(err, exec.ErrNotFound) || errors.As(err, &ee) && ee.ExitCode() == psNoDesktopExit {
					return "", fmt.Errorf("dialog: %w", errors.Join(ErrNoDesktop, err))
				}
				return "", fmt.Errorf("dialog: %w", err)
			}
			return pick(buttons, out), nil
		})
	}
	return d
}

// psNoDesktopExit is the exit status of the script when the session is not interactive: there is no window to show.
const psNoDesktopExit = 3

// psArgs run psDialog: no profile, no prompt, a single-threaded apartment (Windows Forms needs one).
var psArgs = []string{"-NoProfile", "-NonInteractive", "-STA", "-Command", psDialog}

// psDialog shows the window. Escape and the close box press the first button; the last is the default. It prints
// the label of the button pressed. It is a constant: see winDesktop.
const psDialog = `$ErrorActionPreference = 'Stop'
if (-not [Environment]::UserInteractive) { exit 3 }
[Console]::InputEncoding = [System.Text.Encoding]::UTF8
$j = [Console]::In.ReadToEnd() | ConvertFrom-Json
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
[System.Windows.Forms.Application]::EnableVisualStyles()
$bs = @($j.buttons)
$script:r = [string]$bs[0]
$f = New-Object System.Windows.Forms.Form
$f.Text = [string]$j.title
$f.StartPosition = 'CenterScreen'
$f.FormBorderStyle = 'FixedDialog'
$f.MaximizeBox = $false
$f.MinimizeBox = $false
$f.TopMost = $true
$f.ShowInTaskbar = $true
$l = New-Object System.Windows.Forms.Label
$l.AutoSize = $true
$l.UseMnemonic = $false
$l.MaximumSize = New-Object System.Drawing.Size(488, 0)
$l.Location = New-Object System.Drawing.Point(16, 16)
$l.Text = [string]$j.text
$f.Controls.Add($l)
$top = 16 + $l.PreferredHeight + 24
$f.ClientSize = New-Object System.Drawing.Size(520, ($top + 40))
$x = 520 - 16
for ($i = $bs.Count - 1; $i -ge 0; $i--) {
  $b = New-Object System.Windows.Forms.Button
  $b.UseMnemonic = $false
  $b.Text = [string]$bs[$i]
  $b.Size = New-Object System.Drawing.Size(154, 30)
  $x = $x - 154
  $b.Location = New-Object System.Drawing.Point($x, $top)
  $x = $x - 8
  $b.Add_Click({ param($s, $e) $script:r = $s.Text; $s.FindForm().Close() })
  $f.Controls.Add($b)
  if ($i -eq $bs.Count - 1) { $f.AcceptButton = $b }
  if ($i -eq 0) { $f.CancelButton = $b }
}
[void]$f.ShowDialog()
Write-Output $script:r
`

// asciiJSON is v as JSON with every character outside printable ASCII written as a \u escape.
func asciiJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, r := range string(b) {
		switch {
		case r < 0x7f && r >= 0x20:
			sb.WriteRune(r)
		case r > 0xffff:
			r -= 0x10000
			fmt.Fprintf(&sb, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			fmt.Fprintf(&sb, `\u%04x`, r)
		}
	}
	return sb.String(), nil
}
