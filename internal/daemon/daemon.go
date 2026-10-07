// Package daemon runs the long-lived Rubi process: one instance per $RUBI_HOME, serving MCP sessions
// over a Unix socket that only the owning user can open.
package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Deikus-LXXVII/rubi/internal/core"
	"github.com/Deikus-LXXVII/rubi/internal/integrity"
	"github.com/Deikus-LXXVII/rubi/internal/mcpserver"
	"github.com/Deikus-LXXVII/rubi/internal/panelapi"
	"github.com/Deikus-LXXVII/rubi/internal/paths"
	"github.com/Deikus-LXXVII/rubi/internal/tunnel"
	"github.com/Deikus-LXXVII/rubi/internal/version"

	_ "github.com/Deikus-LXXVII/rubi/internal/integrations/icloudmail" // built-in integrations
)

var ErrAlreadyRunning = errors.New("rubi daemon is already running")

func Run(ctx context.Context, layout paths.Layout) error {
	if err := layout.Ensure(); err != nil {
		return err
	}
	lock, err := acquireLock(layout.Lock())
	if err != nil {
		return err
	}
	defer lock.Close()

	c, err := core.Open(layout)
	if err != nil {
		return err
	}
	if err := receiveHandoff(c); err != nil {
		log.Printf("update handoff: %v (starting locked)", err)
	}
	srv := mcpserver.New(c)

	_ = os.Remove(layout.Socket()) // stale socket from a crashed run; the lock proves no one else owns it
	ln, err := net.Listen("unix", layout.Socket())
	if err != nil {
		return err
	}
	if err := os.Chmod(layout.Socket(), 0o600); err != nil {
		ln.Close()
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.SetShutdown(cancel)
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	tunnelDone, err := startPanelTransport(ctx, c, layout)
	if err != nil {
		ln.Close()
		return err
	}
	go checkUpdates(ctx, c)

	go func() {
		r := integrity.Check(ctx, version.Version)
		c.SetIntegrity(r)
		log.Printf("binary self-check: %s (%s)", r.Status, r.Detail)
	}()

	c.Audit.Record("daemon.started", nil)
	log.Printf("rubi %s daemon started: state=%s instance=%s socket=%s", version.Version, c.State(),
		c.ID.InstanceID, layout.Socket())

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Printf("accept: %v", err)
			continue
		}
		go serve(ctx, srv.MCP(), conn)
	}

	if exe, ok := c.PendingHandoff(); ok {
		err := handoff(c, layout, lock, tunnelDone, exe)
		// Only reached if exec failed. The new binary is installed; the next start runs it, locked.
		log.Printf("restart into the update failed: %v", err)
	}
	c.Lock()
	_ = os.Remove(layout.Socket())
	log.Printf("rubi daemon stopped")
	return nil
}

// checkUpdates looks for new releases shortly after start and then every six hours.
func checkUpdates(ctx context.Context, c *core.Core) {
	if version.Version == "dev" {
		return
	}
	delay := 30 * time.Second
	if d, err := time.ParseDuration(os.Getenv("RUBI_UPDATE_CHECK_DELAY")); err == nil && strings.HasSuffix(version.Version, "-test") {
		delay = d
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		info := c.CheckForUpdate(ctx)
		if info.Available {
			log.Printf("update available: %s -> %s", info.Current, info.Latest)
		}
		delay = 6 * time.Hour
	}
}

type handoffPayload struct {
	DEK          string `json:"dek"`
	VaultVersion uint64 `json:"vault_version"`
}

// handoff replaces this process with the freshly installed, verified binary and passes it the open vault
// key through an inherited pipe, so the update doesn't lock Rubi. The key never touches disk or the
// environment; only the pipe's descriptor number is passed.
func handoff(c *core.Core, layout paths.Layout, lock *os.File, tunnelDone <-chan struct{}, exe string) error {
	select { // let the tunnel process exit so it isn't orphaned
	case <-tunnelDone:
	case <-time.After(5 * time.Second):
	}
	if c.State() != core.Unlocked {
		return syscall.Exec(exe, []string{exe, "daemon"}, withoutHandoffEnv())
	}
	dek, err := c.Vault.Key()
	if err != nil {
		return err
	}
	ver, _ := c.Vault.Version()
	payload, _ := json.Marshal(handoffPayload{DEK: base64.RawURLEncoding.EncodeToString(dek), VaultVersion: ver})
	for i := range dek {
		dek[i] = 0
	}
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	w.Close()
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, r.Fd(), syscall.F_SETFD, 0); errno != 0 {
		return errno
	}
	_ = os.Remove(layout.Socket())
	_ = lock.Close()
	log.Printf("restarting into %s", exe)
	env := append(withoutHandoffEnv(), "RUBI_HANDOFF_FD="+strconv.Itoa(int(r.Fd())))
	return syscall.Exec(exe, []string{exe, "daemon"}, env)
}

func withoutHandoffEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "RUBI_HANDOFF_FD=") {
			env = append(env, kv)
		}
	}
	return env
}

// receiveHandoff unlocks with the key passed by the previous process after an update.
func receiveHandoff(c *core.Core) error {
	fdStr := os.Getenv("RUBI_HANDOFF_FD")
	if fdStr == "" {
		return nil
	}
	_ = os.Unsetenv("RUBI_HANDOFF_FD")
	fd, err := strconv.Atoi(fdStr)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), "handoff")
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return err
	}
	var p handoffPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	dek, err := base64.RawURLEncoding.DecodeString(p.DEK)
	if err != nil {
		return err
	}
	defer func() {
		for i := range dek {
			dek[i] = 0
		}
	}()
	if err := c.Unlock(dek, p.VaultVersion, "update", ""); err != nil {
		return err
	}
	log.Printf("unlocked after update to %s", version.Version)
	return nil
}

// startPanelTransport serves the panel API on loopback and exposes it through the configured transport:
// RUBI_PUBLIC_URL if set (e.g. Tailscale serve), otherwise a Cloudflare quick tunnel.
func startPanelTransport(ctx context.Context, c *core.Core, layout paths.Layout) (<-chan struct{}, error) {
	done := make(chan struct{})
	addr := "127.0.0.1:0"
	if p := os.Getenv("RUBI_PANEL_API_PORT"); p != "" {
		addr = "127.0.0.1:" + p
	}
	pln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("panel API listener: %w", err)
	}
	hs := &http.Server{Handler: panelapi.New(c).Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	go func() { _ = hs.Serve(pln) }()
	go func() {
		<-ctx.Done()
		_ = hs.Close()
	}()
	target := "http://" + pln.Addr().String()
	log.Printf("panel API on %s", target)

	if u := os.Getenv("RUBI_PUBLIC_URL"); u != "" {
		c.SetEndpoint(u)
		log.Printf("panel transport: fixed URL %s", u)
		close(done)
		return done, nil
	}
	bin, err := tunnel.FindCloudflared(layout.Home)
	if err != nil {
		c.SetTransportError(err.Error())
		log.Printf("panel transport unavailable: %v", err)
		close(done)
		return done, nil
	}
	m := &tunnel.Manager{Binary: bin, Target: target, OnURL: c.SetEndpoint, Logf: log.Printf}
	go func() {
		m.Run(ctx)
		close(done)
	}()
	return done, nil
}

func serve(ctx context.Context, s *mcp.Server, conn net.Conn) {
	defer conn.Close()
	ss, err := s.Connect(ctx, &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
	if err != nil {
		log.Printf("mcp session: %v", err)
		return
	}
	_ = ss.Wait()
}

// acquireLock takes an exclusive, non-blocking flock so only one daemon runs per $RUBI_HOME.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return f, nil
}
