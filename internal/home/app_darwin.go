package home

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

// The Mac app and its LaunchAgent. What Finder does with the app: it runs Contents/MacOS/rubi-home with no arguments
// (very old macOS adds -psn_...). The other systems have no app: app_notmac.go has the same functions, which say no.

// AgentLabel is the LaunchAgent's label (and the app's bundle identifier).
const AgentLabel = "com.rubi-project.home"

const launchctl = "/bin/launchctl"

// IsAppLaunch says whether Finder opened the app: no arguments (or only -psn_*), from a *.app/Contents/MacOS/ folder
// whose Contents/Info.plist names this app. A bare run of the binary elsewhere keeps printing its usage.
func IsAppLaunch(args []string, exe string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "-psn_") {
			return false
		}
	}
	return InAppBundle(exe) && bundleID(filepath.Join(filepath.Dir(filepath.Dir(exe)), "Info.plist")) == AgentLabel
}

// bundleID is the CFBundleIdentifier of an XML Info.plist (the build script writes one), or "".
func bundleID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	dec := xml.NewDecoder(io.LimitReader(f, 64<<10))
	var key, text string
	var in, wantValue bool
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		switch e := tok.(type) {
		case xml.StartElement:
			if e.Name.Local == "key" || e.Name.Local == "string" {
				in, text = true, ""
			}
		case xml.CharData:
			if in {
				text += string(e)
			}
		case xml.EndElement:
			switch e.Name.Local {
			case "key":
				key, in, wantValue = text, false, true
			case "string":
				in = false
				if wantValue && key == "CFBundleIdentifier" {
					return strings.TrimSpace(text)
				}
				wantValue = false
			}
		}
	}
}

// InAppBundle is true for <name>.app/Contents/MacOS/<file>.
func InAppBundle(exe string) bool {
	if exe == "" {
		return false
	}
	macos := filepath.Dir(exe)
	contents := filepath.Dir(macos)
	return filepath.Base(macos) == "MacOS" && filepath.Base(contents) == "Contents" &&
		strings.HasSuffix(filepath.Base(filepath.Dir(contents)), ".app")
}

// AgentPlistTemplate is the LaunchAgent that panel/install-home.sh writes, with its two variables: Bin is the
// installed binary and Logs the log folder. A test keeps the script and this one the same.
const AgentPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.rubi-project.home</string>
  <key>ProgramArguments</key><array><string>{{xml .Bin}}</string><string>run</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>{{xml .Logs}}/rubi-home.log</string>
  <key>StandardErrorPath</key><string>{{xml .Logs}}/rubi-home.log</string>
</dict>
</plist>
`

var agentPlist = template.Must(template.New("agent").Funcs(template.FuncMap{"xml": func(s string) (string, error) {
	var b bytes.Buffer
	err := xml.EscapeText(&b, []byte(s))
	return b.String(), err
}}).Parse(AgentPlistTemplate))

// AgentPlist is the LaunchAgent file for a binary and a log folder.
func AgentPlist(bin, logs string) (string, error) {
	var b bytes.Buffer
	err := agentPlist.Execute(&b, struct{ Bin, Logs string }{bin, logs})
	return b.String(), err
}

// writeAgent writes the LaunchAgent file unless it is already exactly this; it reports whether it wrote. A symlink
// in its place is replaced by a regular file (the rename does that) and logged.
func writeAgent(path, bin, logs string, logf func(string, ...any)) (bool, error) {
	want, err := AgentPlist(bin, logs)
	if err != nil {
		return false, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().IsRegular() {
			if have, err := os.ReadFile(path); err == nil && string(have) == want {
				return false, nil
			}
		} else {
			logf("rubi-home app: %s is not a regular file; replacing it", path)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".rubi-home-agent-*")
	if err != nil {
		return false, err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.WriteString(want); err != nil {
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		return false, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, err
	}
	ok = true
	return true, nil
}

// loadAgent starts the agent: a new one (or a changed file) is booted out, if there, and bootstrapped; an
// agent that is loaded with the same file is restarted only when the binary changed. It reports whether the
// agent was loaded before and whether this call started (or restarted) it.
func loadAgent(ctx context.Context, env AutostartEnv, launch func(context.Context, ...string) error, plist string, binChanged, plistChanged bool) (loaded, restarted bool, err error) {
	domain := fmt.Sprintf("gui/%d", env.UID)
	loaded = launch(ctx, "print", domain+"/"+AgentLabel) == nil
	if !loaded || plistChanged {
		if err := launch(ctx, "bootout", domain, plist); err != nil {
			env.Logf("rubi-home app: nothing to unload: %v", err) // normal for a first install
		}
		if err := launch(ctx, "bootstrap", domain, plist); err != nil {
			// The unload can still be settling, or the user switched the agent off in Login Items: enable it, then once more.
			env.Logf("rubi-home app: loading the agent failed, enabling it and trying again: %v", err)
			if eerr := launch(ctx, "enable", domain+"/"+AgentLabel); eerr != nil {
				env.Logf("rubi-home app: enabling the agent: %v", eerr)
			}
			env.Sleep(time.Second)
			if err := launch(ctx, "bootstrap", domain, plist); err != nil {
				return loaded, false, fmt.Errorf("loading the agent: %w", err)
			}
		}
		return loaded, true, nil
	}
	if binChanged {
		if err := launch(ctx, "kickstart", "-k", domain+"/"+AgentLabel); err != nil {
			return loaded, false, fmt.Errorf("restarting the agent: %w", err)
		}
		return loaded, true, nil
	}
	return loaded, false, nil
}
