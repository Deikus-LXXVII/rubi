package sshtarget

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
)

const dialTimeout = 15 * time.Second

func pemBytes(b *pem.Block) []byte { return pem.EncodeToMemory(b) }

var errScanned = errors.New("host key read")

// ScanResult is ssh.scan's answer.
type ScanResult struct {
	Fingerprint string `json:"fingerprint"`
	KeyType     string `json:"key_type"`
}

// Scan dials host:port from this computer and reads the host key; there is no authentication.
func (s *Store) Scan(ctx context.Context, host string, port int) (ScanResult, error) {
	port, err := ValidatePort(port)
	if err != nil {
		return ScanResult{}, err
	}
	if err := ValidateHost(host); err != nil {
		return ScanResult{}, err
	}
	k, err := scanKey(ctx, s.dial, net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return ScanResult{}, err
	}
	return ScanResult{Fingerprint: ssh.FingerprintSHA256(k), KeyType: k.Type()}, nil
}

// scanKey connects, takes the host key the server offers first and hangs up before any authentication.
func scanKey(ctx context.Context, dial Dialer, addr string) (ssh.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("can't reach %s: %w", addr, errText(err))
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	var got ssh.PublicKey
	cfg := &ssh.ClientConfig{User: "rubi-scan",
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error { got = k; return errScanned }}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err == nil { // can't happen: the callback always refuses
		c.Close()
		_, _ = chans, reqs
	}
	if got == nil {
		return nil, fmt.Errorf("%s didn't present an SSH host key: %w", addr, errText(err))
	}
	return got, nil
}

// errText keeps an error's text without wrapping chains that could carry more than a message.
func errText(err error) error {
	if err == nil {
		return errors.New("unknown error")
	}
	return errors.New(err.Error())
}
