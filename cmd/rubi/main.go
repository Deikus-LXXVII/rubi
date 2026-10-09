// Command rubi is the Rubi-Project daemon, MCP entry point, and CLI.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Deikus-LXXVII/rubi/internal/daemon"
	"github.com/Deikus-LXXVII/rubi/internal/mcpproxy"
	"github.com/Deikus-LXXVII/rubi/internal/panelclient"
	"github.com/Deikus-LXXVII/rubi/internal/paths"
	"github.com/Deikus-LXXVII/rubi/internal/update"
	"github.com/Deikus-LXXVII/rubi/internal/version"
)

const usage = `rubi — self-hosted integrations for AI agents, gated by your approval

Usage:
  rubi mcp        MCP server over stdio (register this with your agent); starts the daemon if needed
  rubi daemon     run the daemon in the foreground
  rubi status     print Rubi's state
  rubi version    print the version
  rubi update [--check | <version>]
                  check for or install a signed release (the agent's rubi_update tool does this without
                  locking Rubi; this command restarts it locked)
  rubi rollback   go back to the previous version
  rubi transport [gateway | relays | tailscale]
                  show or choose how the panel reaches Rubi (Rubi Gateway, public relays, or your own
                  Tailscale network); Rubi restarts locked to apply a change

Development:
  rubi dev-panel <link> [--password-stdin]
                  pair or unlock with a password from the terminal, standing in for the web panel

Environment:
  RUBI_HOME          state directory (default ~/.rubi)
  RUBI_PANEL_ORIGIN  panel origin (default https://rubi-panel.com); only before pairing
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	// "version" needs nothing from the environment: an update runs the new binary with a bare environment
	// to check it starts (internal/update smokeTest), and Rubi v0.6.0 passes only PATH.
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println(version.Version)
		return
	}
	layout, err := paths.Default()
	if err != nil {
		fail(err)
	}
	switch os.Args[1] {
	case "mcp":
		if err := mcpproxy.Run(layout); err != nil {
			fail(err)
		}
	case "daemon":
		if err := daemon.Run(context.Background(), layout); err != nil {
			if errors.Is(err, daemon.ErrAlreadyRunning) {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(0)
			}
			fail(err)
		}
	case "status":
		if err := status(layout); err != nil {
			fail(err)
		}
	case "dev-panel":
		if err := devPanel(os.Args[2:]); err != nil {
			fail(err)
		}
	case "update":
		if err := updateCmd(layout, os.Args[2:]); err != nil {
			fail(err)
		}
	case "transport":
		if len(os.Args) < 3 {
			fmt.Println("transport: " + daemon.LoadTransport(layout).Transport)
			return
		}
		if err := daemon.SaveTransport(layout, os.Args[2]); err != nil {
			fail(err)
		}
		stopDaemon(layout)
		fmt.Println("transport: " + os.Args[2] + " (Rubi restarts locked on its next use; give the user the new unlock link)")
	case "rollback":
		exe, err := selfPath()
		if err == nil {
			err = update.Rollback(exe)
		}
		if err != nil {
			fail(err)
		}
		stopDaemon(layout)
		fmt.Println("Rolled back. Rubi restarts locked on its next use.")
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

// status talks to the daemon over MCP, exactly like the agent does.
func status(layout paths.Layout) error {
	conn, err := mcpproxy.Dial(layout)
	if err != nil {
		return err
	}
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "rubi-cli", Version: version.Version}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
	if err != nil {
		return err
	}
	defer session.Close()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "rubi_status"})
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(res.StructuredContent, "", "  ")
	fmt.Println(string(b))
	return nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "rubi:", err)
	os.Exit(1)
}

// devPanel stands in for the web panel during development: it speaks the same protocol, with a password.
func devPanel(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: rubi dev-panel <link> [--password-stdin]")
	}
	l, err := panelclient.ParseLink(args[0])
	if err != nil {
		return err
	}
	c, err := panelclient.New(l)
	if err != nil {
		return err
	}
	var h panelclient.Hello
	if err := c.Call("hello", nil, &h); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Connected to %s (fingerprint %s), state %s\n", h.Instance, h.Fingerprint, h.State)

	fromStdin := len(args) > 1 && args[1] == "--password-stdin"
	in := bufio.NewReader(os.Stdin)
	read := func(prompt string) (string, error) {
		if fromStdin {
			line, err := in.ReadString('\n')
			return strings.TrimRight(line, "\r\n"), err
		}
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(b), err
	}

	switch {
	case l.Pairing != "":
		pw, err := read("New password: ")
		if err != nil {
			return err
		}
		if !fromStdin {
			again, err := read("Repeat: ")
			if err != nil {
				return err
			}
			if again != pw {
				return errors.New("passwords do not match")
			}
		}
		if len(pw) < 12 {
			return errors.New("use at least 12 characters")
		}
		v, err := c.PairWithPassword(pw)
		if err != nil {
			return err
		}
		fmt.Printf("Paired and unlocked (vault version %d)\n", v)
	default:
		pw, err := read("Password: ")
		if err != nil {
			return err
		}
		v, err := c.UnlockWithPassword(pw, 0)
		if err != nil {
			return err
		}
		fmt.Printf("Unlocked (vault version %d)\n", v)
	}
	return nil
}

func selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return exe, nil
}

// updateCmd is the manual update path. It verifies exactly like the in-daemon update, then restarts the
// daemon, which comes back locked.
func updateCmd(layout paths.Layout, args []string) error {
	ctx := context.Background()
	target := ""
	if len(args) > 0 && args[0] != "--check" {
		target = args[0]
	}
	if target == "" {
		rel, err := update.Latest(ctx, version.Version)
		if err != nil {
			return err
		}
		if !update.Newer(rel.Version, version.Version) {
			fmt.Printf("Rubi %s is up to date (latest: %s).\n", version.Version, rel.Version)
			return nil
		}
		if len(args) > 0 && args[0] == "--check" {
			fmt.Printf("Update available: %s -> %s\n", version.Version, rel.Version)
			return nil
		}
		target = rel.Version
	}
	// Never back to an older release from here: an old, genuinely signed version may have known bugs.
	// (Going back after a bad update is `rubi rollback`, to the version that was approved before.)
	if version.Version != "dev" && !update.Newer(target, version.Version) {
		return fmt.Errorf("%s is not newer than the installed %s; Rubi only updates forward", target, version.Version)
	}
	exe, err := selfPath()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Downloading and verifying %s...\n", target)
	v, err := update.Download(ctx, version.Version, target, filepath.Dir(exe))
	if err != nil {
		return err
	}
	if err := update.Install(v, exe); err != nil {
		return err
	}
	stopDaemon(layout)
	fmt.Printf("Updated to %s (signature and checksum verified). Rubi restarts on its next use and needs to be unlocked.\n", target)
	return nil
}

// stopDaemon asks a running daemon to exit; the next `rubi mcp` starts the installed binary.
func stopDaemon(layout paths.Layout) {
	b, err := os.ReadFile(layout.Lock())
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
}
