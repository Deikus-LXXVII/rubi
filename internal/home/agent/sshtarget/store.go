// Package sshtarget is Rubi Home's side of the SSH targets of agent roles (docs/design/agent-roles.md, section 9):
// the registry of targets with their pinned host keys, the Ed25519 client keys that never leave Home, the
// pinned-host-key client that runs one command on a target, the stream relay with its limits, and the audit.
// Home is the SSH client; a VM never connects to a target.
package sshtarget

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/crypto/ssh"
)

// Directory and file names under the registry directory (<helper dir>/agent-ssh).
const (
	DirName     = "agent-ssh"
	targetsFile = "targets.json"
	keysDir     = "keys"
)

// Limits of the registry.
const (
	MaxTargets         = 64
	MaxAllowedCommands = 16
	MaxCommandLen      = 8192 // a command a stream may run
	MaxPatternLen      = 200
)

var (
	nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`) // as roles.sshRE
	hostRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.:-]{0,252}$`)
	userRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,31}$`)
	fpRE   = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
)

// ValidName says whether s can name a target.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Target is one registered target. HostKey is the pinned host key (authorized_keys format, without a comment);
// Fingerprint is its SHA256 fingerprint.
type Target struct {
	Name            string   `json:"name"`
	Host            string   `json:"host"`
	Port            int      `json:"port"`
	User            string   `json:"user"`
	Fingerprint     string   `json:"fingerprint"`
	HostKey         string   `json:"host_key"`
	AllowedCommands []string `json:"allowed_commands,omitempty"`
	AskEachTime     bool     `json:"ask_each_time,omitempty"`
	CreatedAt       string   `json:"created_at"`
}

// Addr is host:port.
func (t Target) Addr() string { return net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) }

// View is a target as the list op shows it: nothing of the client key (not even its public half).
type View struct {
	Name            string   `json:"name"`
	Host            string   `json:"host"`
	Port            int      `json:"port"`
	User            string   `json:"user"`
	Fingerprint     string   `json:"fingerprint"`
	KeyType         string   `json:"key_type"`
	AllowedCommands []string `json:"allowed_commands"`
	AskEachTime     bool     `json:"ask_each_time"`
	CreatedAt       string   `json:"created_at"`
	// HostKeyChanged is set from the last dial that found a different host key ("" = none since Home started
	// or since the last good dial).
	HostKeyChanged string `json:"host_key_changed,omitempty"`
}

// Dialer opens the TCP connection to a target or a host being scanned (a seam for tests).
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// Store is the target registry and the key files.
type Store struct {
	dir  string
	dial Dialer
	now  func() time.Time

	mu      sync.Mutex
	targets map[string]Target
	changed map[string]string // target -> when a dial found another host key
}

// Open opens (creating it, 0700) the registry in dir. dial may be nil (a plain TCP dial); now may be nil.
func Open(dir string, dial Dialer, now func() time.Time) (*Store, error) {
	if dial == nil {
		dial = (&net.Dialer{Timeout: dialTimeout}).DialContext
	}
	if now == nil {
		now = time.Now
	}
	s := &Store{dir: dir, dial: dial, now: now, targets: map[string]Target{}, changed: map[string]string{}}
	for _, d := range []string{dir, filepath.Join(dir, keysDir)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return nil, err
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, targetsFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		var f struct {
			Targets []Target `json:"targets"`
		}
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, fmt.Errorf("%s: %w", targetsFile, err)
		}
		for _, t := range f.Targets {
			if nameRE.MatchString(t.Name) {
				s.targets[t.Name] = t
			}
		}
	}
	return s, nil
}

// Dir is the registry directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) saveLocked() error {
	var f struct {
		Targets []Target `json:"targets"`
	}
	f.Targets = []Target{}
	for _, t := range s.targets {
		f.Targets = append(f.Targets, t)
	}
	sort.Slice(f.Targets, func(i, j int) bool { return f.Targets[i].Name < f.Targets[j].Name })
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, targetsFile), b, 0o600)
}

func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// Get returns a target.
func (s *Store) Get(name string) (Target, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.targets[name]
	return t, ok
}

// List returns the targets by name.
func (s *Store) List() []View {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []View{}
	for _, t := range s.targets {
		v := View{Name: t.Name, Host: t.Host, Port: t.Port, User: t.User, Fingerprint: t.Fingerprint,
			AllowedCommands: append([]string{}, t.AllowedCommands...), AskEachTime: t.AskEachTime,
			CreatedAt: t.CreatedAt, HostKeyChanged: s.changed[t.Name]}
		if pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(t.HostKey)); err == nil {
			v.KeyType = pk.Type()
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// markChanged records (or clears) that a dial found another host key.
func (s *Store) markChanged(name string, changed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if changed {
		s.changed[name] = s.now().UTC().Format(time.RFC3339)
	} else {
		delete(s.changed, name)
	}
}

// RegisterArgs is ssh.register.
type RegisterArgs struct {
	Name            string   `json:"name"`
	Host            string   `json:"host"`
	Port            int      `json:"port"`
	User            string   `json:"user"`
	Fingerprint     string   `json:"fingerprint"`
	AllowedCommands []string `json:"allowed_commands"`
	AskEachTime     bool     `json:"ask_each_time"`
}

func badText(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) }) >= 0
}

// ValidatePort returns the port (22 when 0).
func ValidatePort(p int) (int, error) {
	if p == 0 {
		return 22, nil
	}
	if p < 1 || p > 65535 {
		return 0, errors.New("the port is a number from 1 to 65535")
	}
	return p, nil
}

// ValidateHost checks a host name or address.
func ValidateHost(h string) error {
	if !hostRE.MatchString(h) {
		return errors.New("the host is a name or an address (letters, digits, dots, dashes, colons)")
	}
	return nil
}

// Register pins the live host key of the target, generates its client key and returns the authorized_keys
// line to install on the target. It refuses a name in use, and a live host key whose fingerprint isn't the
// given one. The private key is written (0600) and never returned.
func (s *Store) Register(ctx context.Context, a RegisterArgs) (string, error) {
	port, err := ValidatePort(a.Port)
	if err != nil {
		return "", err
	}
	switch {
	case !nameRE.MatchString(a.Name):
		return "", errors.New("the name is lowercase letters, digits and dashes, up to 32")
	case ValidateHost(a.Host) != nil:
		return "", ValidateHost(a.Host)
	case !userRE.MatchString(a.User):
		return "", errors.New("bad user name")
	case !fpRE.MatchString(a.Fingerprint):
		return "", errors.New("the fingerprint is the host key's SHA256 fingerprint (SHA256:...)")
	case len(a.AllowedCommands) > MaxAllowedCommands:
		return "", fmt.Errorf("more than %d allowed commands", MaxAllowedCommands)
	}
	cmds := []string{}
	for _, c := range a.AllowedCommands {
		c = strings.TrimSpace(c)
		if c == "" || len(c) > MaxPatternLen || badText(c) {
			return "", errors.New("an allowed command is empty, too long or has control characters")
		}
		cmds = append(cmds, c)
	}
	s.mu.Lock()
	_, exists := s.targets[a.Name]
	n := len(s.targets)
	s.mu.Unlock()
	if exists {
		return "", fmt.Errorf("a target named %q exists; remove it first", a.Name)
	}
	if n >= MaxTargets {
		return "", fmt.Errorf("more than %d targets", MaxTargets)
	}
	hk, err := scanKey(ctx, s.dial, net.JoinHostPort(a.Host, strconv.Itoa(port)))
	if err != nil {
		return "", err
	}
	if live := ssh.FingerprintSHA256(hk); live != a.Fingerprint {
		return "", fmt.Errorf("the host key of %s is %s, not the fingerprint given; nothing was registered", net.JoinHostPort(a.Host, strconv.Itoa(port)), live)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	blk, err := ssh.MarshalPrivateKey(priv, "rubi-"+a.Name)
	if err != nil {
		return "", err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	t := Target{Name: a.Name, Host: a.Host, Port: port, User: a.User, Fingerprint: a.Fingerprint,
		HostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hk))), AllowedCommands: cmds, AskEachTime: a.AskEachTime,
		CreatedAt: s.now().UTC().Format(time.RFC3339)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.targets[a.Name]; ok {
		return "", fmt.Errorf("a target named %q exists; remove it first", a.Name)
	}
	kp := s.keyPath(a.Name)
	if err := writeFileAtomic(kp, pemBytes(blk), 0o600); err != nil {
		return "", err
	}
	s.targets[a.Name] = t
	if err := s.saveLocked(); err != nil {
		delete(s.targets, a.Name)
		_ = os.Remove(kp)
		return "", err
	}
	return AuthorizedLine(sp, a.Name, cmds), nil
}

// AuthorizedLine is the public key as an authorized_keys line: restrict (no forwarding, no pty, no agent),
// plus command= when exactly one exact command is allowed.
func AuthorizedLine(pub ssh.PublicKey, name string, cmds []string) string {
	opts := "restrict"
	if len(cmds) == 1 && !strings.HasSuffix(cmds[0], "*") {
		opts += `,command="` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(cmds[0]) + `"`
	}
	return opts + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " rubi-" + name
}

func (s *Store) keyPath(name string) string { return filepath.Join(s.dir, keysDir, name) }

// Remove deletes a target and its key.
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.targets[name]; !ok {
		return fmt.Errorf("no target %q", name)
	}
	delete(s.targets, name)
	delete(s.changed, name)
	err := s.saveLocked()
	if e := os.Remove(s.keyPath(name)); e != nil && !errors.Is(e, os.ErrNotExist) && err == nil {
		err = e
	}
	return err
}

// signer loads the client key of a target; the file must be private to Home.
func (s *Store) signer(name string) (ssh.Signer, error) {
	p := s.keyPath(name)
	st, err := os.Lstat(p)
	if err != nil {
		return nil, errors.New("the target has no key")
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("the target's key file isn't private (0600); refusing to use it")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, errors.New("the target's key can't be read")
	}
	sg, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return nil, errors.New("the target's key file is malformed")
	}
	return sg, nil
}
