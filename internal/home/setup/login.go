package setup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// loginHosts are the hosts a sign-in link may point to (and that login.open opens).
var loginHosts = []string{"claude.com", "claude.ai", "platform.claude.com", "console.anthropic.com"}

// loginLimit is how long a sign-in waits.
var loginLimit = 15 * time.Minute

type loginState struct {
	url       string
	wantsCode bool
	codeSent  bool
	conn      agent.Conn
}

// loginURL validates a sign-in link: https, one of loginHosts, no user info, no port.
func loginURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || !slices.Contains(loginHosts, u.Hostname()) || len(raw) > 4096 {
		return "", errors.New("the sign-in link doesn't point to a Claude or Anthropic sign-in page")
	}
	return raw, nil
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

// loginEnv: the config of the sign-in's claude lives in the container's tmpfs, not in the shared volume.
var loginEnv = map[string]string{"CLAUDE_CONFIG_DIR": "/tmp/claude-signin"}

// runLogin signs the computer in, once for all projects: it starts the sign-in container (no project, the
// shared volume read-write, a proxy that allows only the sign-in hosts) and runs the image's helper in it,
// which runs `claude setup-token` under a terminal and tells Home on stdout only `URL <link>`, `CODE?`,
// `SAVED` or `FAILED <reason>`. Home shows the link, passes the code the user pastes to the helper's stdin and
// never sees the token. Without a method it only checks the sign-in; skipIfSignedIn leaves a valid one alone.
func (s *Setup) runLogin(ctx context.Context, j *job, method string, skipIfSignedIn bool) outcome {
	log := j.log(s)
	defer s.forgetAuth()
	if method == "" || skipIfSignedIn {
		st, err := s.opt.Agents.AuthStatus(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return outcome{Todo, "Cancelled."}
			}
			return outcome{Failed, "Couldn't check the sign-in: " + safeText(err.Error())}
		}
		if st.SignedIn && !st.Expired(s.opt.Now()) {
			return outcome{Done, "Signed in."}
		}
		if method == "" {
			return outcome{NeedsUser, "Not signed in. Choose how to sign in."}
		}
	}
	log("Starting the sign-in container...")
	sess, err := s.opt.Agents.StartAuth(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return outcome{Todo, "Cancelled."}
		}
		return outcome{Failed, "Couldn't start the sign-in container: " + safeText(err.Error())}
	}
	defer sess.Close() // ends the helper, claude and the proxy, whatever happens
	lctx, cancel := context.WithTimeout(ctx, loginLimit)
	defer cancel()
	conn, err := sess.Exec(lctx, []string{agent.AuthHelper}, loginEnv)
	if err != nil {
		return outcome{Failed, "Couldn't start the sign-in: " + safeText(err.Error())}
	}
	defer conn.Close()
	st := &loginState{conn: conn}
	s.setLogin(j, st)
	defer s.setLogin(j, nil)
	log("Waiting for the sign-in link...")

	var bad, saved bool
	var failure string
	read := make(chan struct{})
	go func() {
		defer close(read)
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 0, 4096), 16<<10)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			switch {
			case strings.HasPrefix(line, "URL "):
				u, verr := loginURL(strings.TrimSpace(line[4:]))
				s.mu.Lock()
				if verr != nil {
					bad = true
				} else if st.url == "" {
					st.url = u
				}
				s.mu.Unlock()
				if verr != nil {
					_ = conn.Close()
					return
				}
				log("The sign-in link is ready.") // the link itself is never logged
			case line == "CODE?":
				s.mu.Lock()
				st.wantsCode = true
				s.mu.Unlock()
			case line == "SAVED":
				s.mu.Lock()
				saved = true
				s.mu.Unlock()
			case strings.HasPrefix(line, "FAILED"):
				s.mu.Lock()
				failure = safeText(strings.TrimSpace(strings.TrimPrefix(line, "FAILED")))
				if failure == "" {
					failure = "no details"
				}
				s.mu.Unlock()
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
	gotURL, wasSaved, why := st.url != "", saved, failure
	s.mu.Unlock()
	switch {
	case ctx.Err() != nil:
		return outcome{Todo, "Cancelled."}
	case bad:
		return outcome{Failed, "The sign-in link doesn't point to a Claude or Anthropic sign-in page, so it wasn't shown."}
	case lctx.Err() != nil:
		return outcome{Failed, fmt.Sprintf("The sign-in waited %d minutes and was stopped.", int(loginLimit.Minutes()))}
	case !wasSaved:
		switch {
		case why != "":
			return outcome{Failed, "The sign-in failed: " + why + "."}
		case !gotURL:
			return outcome{Failed, "The sign-in ended without showing a link."}
		}
		return outcome{Failed, "The sign-in ended without a token."}
	}
	// The helper says it saved; look for the token the way every status does, without reading it.
	if got, err := s.opt.Agents.AuthStatus(ctx); err != nil || !got.SignedIn {
		return outcome{Failed, "The sign-in finished, but no token is stored."}
	}
	return outcome{Done, "Signed in."}
}

// runSignOut deletes the shared token for every project. It can't revoke it: that is done at claude.ai.
func (s *Setup) runSignOut(ctx context.Context, j *job) outcome {
	defer s.forgetAuth()
	j.log(s)("Deleting the stored sign-in...")
	if err := s.opt.Agents.AuthSignOut(ctx); err != nil {
		if ctx.Err() != nil {
			return outcome{Todo, "Cancelled."}
		}
		return outcome{Failed, "Couldn't delete the sign-in: " + safeText(err.Error())}
	}
	if got, err := s.opt.Agents.AuthStatus(ctx); err != nil || got.SignedIn {
		return outcome{Failed, "The sign-in is still stored."}
	}
	return outcome{Done, "Signed out on this computer. To revoke the token everywhere, also remove it in your Claude account settings at claude.ai."}
}

// LoginCode writes the code Claude asked for to the running sign-in. The sign-in is one for the whole
// computer, so the project argument of the operation is ignored. Nothing is kept or logged.
func (s *Setup) LoginCode(code string) error {
	if !ValidCode(code) {
		return errors.New("the code must be 1 to 512 visible characters without spaces")
	}
	s.mu.Lock()
	var st *loginState
	if s.job != nil && s.job.step == StepLogin && s.job.login != nil && s.job.login.url != "" {
		st = s.job.login
		st.wantsCode, st.codeSent = false, true
	}
	s.mu.Unlock()
	if st == nil {
		return errors.New("no sign-in is waiting for a code")
	}
	if _, err := st.conn.Write([]byte(code + "\n")); err != nil {
		return errors.New("the sign-in isn't accepting a code any more")
	}
	return nil
}

// LoginOpen opens the running sign-in's link in the Mac's default browser.
func (s *Setup) LoginOpen(ctx context.Context) error {
	if !s.mac() {
		return errNotMac
	}
	s.mu.Lock()
	raw := ""
	if s.job != nil && s.job.login != nil {
		raw = s.job.login.url
	}
	s.mu.Unlock()
	if raw == "" {
		return errors.New("no sign-in is waiting")
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
