// Package sshtest is an in-process SSH server for tests: it accepts the authorized client keys, runs exec
// requests through a handler, and refuses everything else (shells, ptys, forwarding).
package sshtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"strconv"
	"sync"

	"golang.org/x/crypto/ssh"
)

// Handler runs one exec request. Its return value is the exit status. It should return when stdin ends or
// quit is closed (the connection went away).
type Handler func(cmd string, stdin io.Reader, stdout, stderr io.Writer) int

// Server is a test SSH server on 127.0.0.1.
type Server struct {
	Host string
	Port int
	// Handler answers exec requests (nil: echo the command to stdout, exit 0).
	Handler Handler

	ln      net.Listener
	cfg     *ssh.ServerConfig
	hostKey ssh.Signer

	mu       sync.Mutex
	authed   []ssh.PublicKey
	commands []string
	users    []string
	conns    map[net.Conn]bool
	logins   int
}

// New starts a server with a fresh Ed25519 host key.
func New() (*Server, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	hk, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	return NewWithKey(hk)
}

// NewWithKey starts a server with the given host key.
func NewWithKey(hk ssh.Signer) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, hostKey: hk, conns: map[net.Conn]bool{}}
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	s.Host = h
	s.Port, _ = strconv.Atoi(p)
	s.cfg = &ssh.ServerConfig{PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, a := range s.authed {
			if string(a.Marshal()) == string(k.Marshal()) {
				s.users = append(s.users, c.User())
				s.logins++
				return nil, nil
			}
		}
		return nil, io.ErrUnexpectedEOF
	}}
	s.cfg.AddHostKey(hk)
	go s.accept()
	return s, nil
}

// Addr is host:port.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// HostKey is the server's host key.
func (s *Server) HostKey() ssh.PublicKey { return s.hostKey.PublicKey() }

// Fingerprint is the host key's SHA256 fingerprint.
func (s *Server) Fingerprint() string { return ssh.FingerprintSHA256(s.hostKey.PublicKey()) }

// Authorize lets the key of an authorized_keys line (as ssh.register returns it) log in.
func (s *Server) Authorize(line string) error {
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.authed = append(s.authed, k)
	s.mu.Unlock()
	return nil
}

// Commands are the exec requests received, in order.
func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

// Logins is how many logins succeeded.
func (s *Server) Logins() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins
}

// Users are the user names of the logins.
func (s *Server) Users() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.users...)
}

// Close stops the server and drops its connections.
func (s *Server) Close() {
	_ = s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
}

func (s *Server) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[c] = true
		s.mu.Unlock()
		go s.serve(c)
	}
}

func (s *Server) serve(c net.Conn) {
	defer c.Close()
	sc, chans, reqs, err := ssh.NewServerConn(c, s.cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go func() {
		for r := range reqs {
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
		}
	}()
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go s.session(ch, creqs)
	}
}

func (s *Server) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for r := range reqs {
		if r.Type != "exec" {
			if r.WantReply {
				_ = r.Reply(false, nil)
			}
			continue
		}
		var p struct{ Cmd string }
		if ssh.Unmarshal(r.Payload, &p) != nil {
			_ = r.Reply(false, nil)
			return
		}
		s.mu.Lock()
		s.commands = append(s.commands, p.Cmd)
		s.mu.Unlock()
		_ = r.Reply(true, nil)
		h := s.Handler
		if h == nil {
			h = func(cmd string, _ io.Reader, out, _ io.Writer) int { _, _ = io.WriteString(out, cmd); return 0 }
		}
		code := h(p.Cmd, ch, ch, ch.Stderr())
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
		return
	}
}
