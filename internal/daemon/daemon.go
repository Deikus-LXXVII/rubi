// Package daemon runs the long-lived Rubi process: one instance per $RUBI_HOME, serving MCP sessions
// over a Unix socket that only the owning user can open.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Deikus-LXXVII/rubi/internal/core"
	"github.com/Deikus-LXXVII/rubi/internal/mcpserver"
	"github.com/Deikus-LXXVII/rubi/internal/paths"
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

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		ln.Close()
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

	c.Lock()
	_ = os.Remove(layout.Socket())
	log.Printf("rubi daemon stopped")
	return nil
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
