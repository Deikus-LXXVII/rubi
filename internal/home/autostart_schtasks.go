package home

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"
)

// The Windows form of Autostart: a Scheduled Task of the user, run at that user's logon, in their session, with the
// rights of their ordinary token. A service would run in session 0 with no desktop and no access to the user's files and
// Claude sessions, and a task for "any user" or "at startup" needs an administrator: neither is used.
//
// No elevation, ever. The task is described in XML that names the user in both the trigger and the principal, with
// InteractiveToken and LeastPrivilege; validateTaskXML refuses anything else, schtasks is run with a list of arguments that
// has no /RU, /RP or /RL, and a system that answers "access denied" gets ErrNeedsElevation, not a second try with more
// rights. (Task Scheduler lets a standard user register a task that runs as themselves at their own logon; per-user
// installers rely on it. It is checked on a Windows machine in the CI job, which runs as an administrator, and by hand
// as a standard user.)

const taskName = "Rubi Home"

// winAbs is a Windows absolute path (a drive letter or a UNC share), whatever system this runs on.
var winAbs = regexp.MustCompile(`^([A-Za-z]:\\|\\\\[^\\])`)

// taskXML is the task definition for the installed binary and the user it runs as.
func taskXML(bin, user string) string { return taskXMLNamed(taskName, bin, user) }

func taskXMLNamed(name, bin, user string) string {
	esc := func(s string) string {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Starts Rubi Home when you log in.</Description>
    <URI>\` + esc(name) + `</URI>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + esc(user) + `</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + esc(user) + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>"` + esc(bin) + `"</Command>
      <Arguments>run</Arguments>
    </Exec>
  </Actions>
</Task>
`
}

// taskDoc is the part of the definition the elevation rule is about.
type taskDoc struct {
	Triggers struct {
		Children []struct {
			XMLName xml.Name
			UserID  string `xml:"UserId"`
		} `xml:",any"`
	} `xml:"Triggers"`
	Principals struct {
		Principal []struct {
			UserID    string `xml:"UserId"`
			GroupID   string `xml:"GroupId"`
			LogonType string `xml:"LogonType"`
			RunLevel  string `xml:"RunLevel"`
		} `xml:"Principal"`
	} `xml:"Principals"`
}

// systemAccounts are accounts a task must never run as: they are not the user, and a standard user cannot register them.
var systemAccounts = []string{"system", "nt authority\\system", "s-1-5-18", "local service", "nt authority\\local service", "s-1-5-19",
	"network service", "nt authority\\network service", "s-1-5-20", "administrators", "builtin\\administrators", "s-1-5-32-544", "users", "builtin\\users"}

// validateTaskXML refuses a definition that needs elevation or runs as anyone but the user: it must have exactly one
// trigger, a logon trigger for a named user; one principal, the same user, an interactive token and the least privilege;
// and it must name no group.
func validateTaskXML(x string) error {
	var d taskDoc
	if err := xml.NewDecoder(strings.NewReader(stripEncoding(x))).Decode(&d); err != nil {
		return fmt.Errorf("the task definition can't be read: %w", err)
	}
	if len(d.Triggers.Children) != 1 || d.Triggers.Children[0].XMLName.Local != "LogonTrigger" {
		return errors.New("the task must have one trigger, at the user's logon")
	}
	user := strings.TrimSpace(d.Triggers.Children[0].UserID)
	if user == "" {
		return errors.New("the logon trigger names no user (it would be for every user, which needs an administrator)")
	}
	if len(d.Principals.Principal) != 1 {
		return errors.New("the task must have one principal")
	}
	p := d.Principals.Principal[0]
	switch {
	case strings.TrimSpace(p.GroupID) != "":
		return errors.New("the task runs as a group, which needs an administrator")
	case strings.TrimSpace(p.UserID) != user:
		return errors.New("the task runs as someone other than the user it starts for")
	case p.LogonType != "InteractiveToken":
		return errors.New("the task must run with the user's interactive token")
	case p.RunLevel != "LeastPrivilege":
		return errors.New("the task must run with the least privilege (not the highest, which needs an administrator)")
	}
	for _, a := range systemAccounts {
		if strings.EqualFold(user, a) {
			return fmt.Errorf("the task runs as %s, which is not the user and needs an administrator", user)
		}
	}
	return nil
}

// stripEncoding drops the XML declaration's encoding, so a UTF-8 string with a UTF-16 declaration can be read.
func stripEncoding(x string) string {
	if i := strings.Index(x, "?>"); strings.HasPrefix(x, "<?xml") && i > 0 {
		return x[i+2:]
	}
	return x
}

// utf16LE is s as UTF-16 little-endian with a byte order mark: the encoding schtasks reads a task file in.
func utf16LE(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2, 2+2*len(u))
	b[0], b[1] = 0xff, 0xfe
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	return b
}

// schtasksAutostart makes the form for a user's Task Scheduler.
func schtasksAutostart(env AutostartEnv) Autostart {
	name := env.TaskName
	if name == "" {
		name = taskName
	}
	tool := func() (string, error) {
		if env.SystemDir == "" {
			return "", errors.New("this computer's system folder is unknown, so Rubi Home can't set up its start at login")
		}
		p := env.SystemDir + `\schtasks.exe`
		if _, err := env.LookPath(p); err != nil {
			return "", errors.New("this computer has no Task Scheduler to start Rubi Home at login")
		}
		return p, nil
	}
	// call runs schtasks; "access denied" is not retried with more rights.
	call := func(ctx context.Context, args ...string) (string, error) {
		exe, err := tool()
		if err != nil {
			return "", err
		}
		out, err := env.run(ctx, "", exe, args...)
		if err != nil {
			if denied(out + stderrOf(err)) {
				return out, fmt.Errorf("%w (schtasks %s)", ErrNeedsElevation, args[0])
			}
			return out, fmt.Errorf("schtasks %s: %w", args[0], err)
		}
		return out, nil
	}
	start := func(ctx context.Context) error {
		_, _ = call(ctx, "/End", "/TN", name) // not running is fine
		_, err := call(ctx, "/Run", "/TN", name)
		return err
	}
	return autostartFuncs{
		install: func(ctx context.Context, spec AutostartSpec) (AutostartResult, error) {
			if env.User == "" {
				return AutostartResult{}, errors.New("this computer's user name is unknown, so Rubi Home can't set up its start at login")
			}
			def := taskXMLNamed(name, spec.Bin, env.User)
			if err := validateTaskXML(def); err != nil {
				return AutostartResult{}, fmt.Errorf("%w: %w", ErrNeedsElevation, err)
			}
			if !winAbs.MatchString(spec.Bin) || strings.ContainsAny(spec.Bin, "\"\r\n\x00") {
				return AutostartResult{}, fmt.Errorf("%w: the path of the installed Rubi Home is not one a task can run", errAutostartWrite)
			}
			file := filepath.Join(filepath.Dir(spec.Logs), "rubi-home-task.xml")
			if err := writeFileReplace(file, utf16LE(def), 0o600); err != nil {
				return AutostartResult{}, fmt.Errorf("%w: %w", errAutostartWrite, err)
			}
			defer removeFile(file)
			_, qerr := call(ctx, "/Query", "/TN", name)
			was := qerr == nil
			if _, err := call(ctx, "/Create", "/TN", name, "/XML", file, "/F"); err != nil {
				return AutostartResult{WasLoaded: was}, err
			}
			if err := start(ctx); err != nil {
				return AutostartResult{WasLoaded: was}, err
			}
			return AutostartResult{WasLoaded: was, Restarted: true}, nil
		},
		restart: start,
		remove: func(ctx context.Context) error {
			_, _ = call(ctx, "/End", "/TN", name)
			_, err := call(ctx, "/Delete", "/TN", name, "/F")
			return err
		},
		loaded: func(ctx context.Context) bool { _, err := call(ctx, "/Query", "/TN", name); return err == nil },
	}
}

func denied(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "access is denied") || strings.Contains(t, "access denied") || strings.Contains(t, "requested operation requires elevation")
}
