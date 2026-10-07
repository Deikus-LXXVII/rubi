// Command rubi is the Rubi-Project daemon, MCP entry point, and CLI.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Deikus-LXXVII/rubi/internal/daemon"
	"github.com/Deikus-LXXVII/rubi/internal/mcpproxy"
	"github.com/Deikus-LXXVII/rubi/internal/panelclient"
	"github.com/Deikus-LXXVII/rubi/internal/paths"
	"github.com/Deikus-LXXVII/rubi/internal/version"
)

const usage = `rubi — self-hosted integrations for AI agents, gated by your approval

Usage:
  rubi mcp        MCP server over stdio (register this with your agent); starts the daemon if needed
  rubi daemon     run the daemon in the foreground
  rubi status     print Rubi's state
  rubi version    print the version

Development:
  rubi dev-panel <link> [--password-stdin]
                  pair or unlock with a password from the terminal, standing in for the web panel

Environment:
  RUBI_HOME          state directory (default ~/.rubi)
  RUBI_PANEL_ORIGIN  panel origin (default https://rubi-panel.com)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
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
	case "version", "--version", "-v":
		fmt.Println(version.Version)
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
