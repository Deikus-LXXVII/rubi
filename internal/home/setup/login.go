package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// claudeBin is the Claude Code binary the Agent SDK bundles in the runner image (a Linux arm64 VM).
const claudeBin = "/opt/rubi-runner/node_modules/@anthropic-ai/claude-agent-sdk-linux-arm64/claude"

// loginHosts are the hosts a sign-in link may point to (and that login.open opens).
var loginHosts = []string{"claude.com", "claude.ai", "platform.claude.com", "console.anthropic.com"}

// loginLimit is how long a sign-in waits.
var loginLimit = 15 * time.Minute

type loginState struct {
	project   agent.Project
	url       string
	wantsCode bool
	codeSent  bool
	conn      agent.Conn
}

var (
	urlRE  = regexp.MustCompile(`https://[^\s"'<>]+`)
	ansiRE = regexp.MustCompile(`\x1b(\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\))`)
)

// loginURL validates a sign-in link: https, one of loginHosts, no user info, no port.
func loginURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || !slices.Contains(loginHosts, u.Hostname()) || len(raw) > 4096 {
		return "", errors.New("the sign-in link doesn't point to a Claude or Anthropic sign-in page")
	}
	return raw, nil
}

// findURL returns the first https link in a line of the login's output.
func findURL(line string) string {
	m := urlRE.FindString(ansiRE.ReplaceAllString(line, ""))
	return strings.TrimRight(m, ".,;:)")
}

// ValidCode says whether text can be a sign-in code: 1 to 512 printable ASCII characters, no spaces.
func ValidCode(code string) bool {
	if len(code) == 0 || len(code) > 512 {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] <= ' ' || code[i] > '~' {
			return false
		}
	}
	return true
}

func readAll(ctx context.Context, c agent.Conn, limit int64) []byte {
	var out []byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		out, _ = io.ReadAll(io.LimitReader(c, limit))
	}()
	select {
	case <-done:
		return out
	case <-ctx.Done():
		_ = c.Close()
		<-done
		return nil
	}
}

func (s *Setup) setLogin(j *job, l *loginState) {
	s.mu.Lock()
	j.login = l
	s.mu.Unlock()
}

// runLogin signs a project in: it runs `claude auth login` in the project's VM with pipes, shows the link
// and waits for the user (and for a pasted code if Claude asks) until claude ends. Without a method it only
// checks the sign-in. skipIfSignedIn leaves a project that already is signed in alone.
func (s *Setup) runLogin(ctx context.Context, j *job, p agent.Project, method string, skipIfSignedIn bool) outcome {
	log := j.log(s)
	log("Starting the project's VM...")
	box, err := s.opt.Agents.Sandbox(ctx, p.ID)
	if err != nil {
		if ctx.Err() != nil {
			return outcome{Todo, "Cancelled."}
		}
		return outcome{Failed, "Couldn't start the project's VM: " + safeText(err.Error())}
	}
	if method == "" || skipIfSignedIn {
		ok, err := s.signedIn(ctx, box)
		if err == nil {
			s.mu.Lock()
			s.loginSeen[p.Dir] = ok
			s.mu.Unlock()
		}
		if ok {
			return outcome{Done, "Signed in."}
		}
		if method == "" {
			return outcome{NeedsUser, "Not signed in. Choose how to sign in."}
		}
	}
	flag := "--claudeai"
	if method == "console" {
		flag = "--console"
	}
	lctx, cancel := context.WithTimeout(ctx, loginLimit)
	defer cancel()
	conn, err := box.Exec(lctx, []string{claudeBin, "auth", "login", flag}, nil)
	if err != nil {
		return outcome{Failed, "Couldn't start the sign-in: " + safeText(err.Error())}
	}
	defer conn.Close()
	st := &loginState{project: p, conn: conn}
	s.setLogin(j, st)
	defer s.setLogin(j, nil)
	log("Waiting for the sign-in link...")

	var bad bool
	read := make(chan struct{})
	go func() {
		defer close(read)
		buf := make([]byte, 4096)
		var carry []byte
		for {
			n, err := conn.Read(buf)
			carry = append(carry, buf[:n]...)
			for {
				i := bytes.IndexAny(carry, "\r\n")
				if i < 0 {
					break
				}
				line := string(carry[:i])
				carry = carry[i+1:]
				if strings.Contains(line, "https://") {
					s.mu.Lock()
					first := st.url == ""
					s.mu.Unlock()
					if !first {
						continue
					}
					u, verr := loginURL(findURL(line))
					s.mu.Lock()
					if verr != nil {
						bad = true
					} else {
						st.url = u
					}
					s.mu.Unlock()
					if verr != nil {
						_ = conn.Close()
						return
					}
					log("The sign-in link is ready.") // the link itself is never logged
					continue
				}
				log(line)
			}
			if len(carry) > 8192 {
				carry = carry[len(carry)-4096:]
			}
			if bytes.Contains(bytes.ToLower(carry), []byte("paste code")) {
				s.mu.Lock()
				st.wantsCode = !st.codeSent
				s.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-read:
	case <-lctx.Done():
		_ = conn.Close()
		<-read
	}
	select { // the process may take a moment to end after its output does
	case <-conn.Done():
	case <-time.After(5 * time.Second):
	}
	s.mu.Lock()
	gotURL := st.url != ""
	s.mu.Unlock()
	switch {
	case ctx.Err() != nil:
		return outcome{Todo, "Cancelled."}
	case bad:
		return outcome{Failed, "The sign-in link doesn't point to a Claude or Anthropic sign-in page, so it wasn't shown."}
	case lctx.Err() != nil:
		return outcome{Failed, fmt.Sprintf("The sign-in waited %d minutes and was stopped.", int(loginLimit.Minutes()))}
	}
	ok, _ := s.signedIn(ctx, box)
	s.mu.Lock()
	s.loginSeen[p.Dir] = ok
	s.mu.Unlock()
	switch {
	case ok:
		return outcome{Done, "Signed in."}
	case !gotURL:
		return outcome{Failed, "The sign-in ended without showing a link."}
	}
	return outcome{Failed, "The sign-in ended, but the project isn't signed in."}
}

// LoginCode writes the code Claude asked for to the running sign-in of the project (a project id). Nothing
// is kept or logged.
func (s *Setup) LoginCode(project, code string) error {
	if !ValidCode(code) {
		return errors.New("the code must be 1 to 512 visible characters without spaces")
	}
	s.mu.Lock()
	var st *loginState
	if s.job != nil && s.job.step == StepLogin && s.job.login != nil && s.job.login.project.ID == project && s.job.login.url != "" {
		st = s.job.login
		st.wantsCode, st.codeSent = false, true
	}
	s.mu.Unlock()
	if st == nil {
		return errors.New("no sign-in for that project is waiting for a code")
	}
	if _, err := st.conn.Write([]byte(code + "\n")); err != nil {
		return errors.New("the sign-in isn't accepting a code any more")
	}
	return nil
}

// LoginOpen opens the running sign-in's link in the Mac's default browser.
func (s *Setup) LoginOpen(ctx context.Context, project string) error {
	if !s.mac() {
		return errNotMac
	}
	s.mu.Lock()
	raw := ""
	if s.job != nil && s.job.login != nil && s.job.login.project.ID == project {
		raw = s.job.login.url
	}
	s.mu.Unlock()
	if raw == "" {
		return errors.New("no sign-in for that project is waiting")
	}
	u, err := loginURL(raw)
	if err != nil {
		return err
	}
	if _, err := s.opt.Cmd.Run(ctx, []string{"/usr/bin/open", u}); err != nil {
		return errors.New("couldn't open the browser")
	}
	return nil
}
