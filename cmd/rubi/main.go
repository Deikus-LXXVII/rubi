// Command rubi is the Rubi-Project daemon, MCP entry point, and CLI.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Deikus-LXXVII/rubi/internal/daemon"
	"github.com/Deikus-LXXVII/rubi/internal/mcpproxy"
	"github.com/Deikus-LXXVII/rubi/internal/paths"
	"github.com/Deikus-LXXVII/rubi/internal/version"
)

const usage = `rubi — self-hosted integrations for AI agents, gated by your approval

Usage:
  rubi mcp        MCP server over stdio (register this with your agent); starts the daemon if needed
  rubi daemon     run the daemon in the foreground
  rubi status     print Rubi's state
  rubi version    print the version

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
