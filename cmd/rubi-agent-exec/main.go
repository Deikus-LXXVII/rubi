// rubi-agent-exec is the binaryPath adapter for a trusted, resolved session.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Deikus-LXXVII/rubi/internal/agentbridge"
	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
	"github.com/Deikus-LXXVII/rubi/internal/agenttask"
	"github.com/Deikus-LXXVII/rubi/internal/home"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--task-supervision-version" {
		fmt.Fprintln(os.Stdout, "1")
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "--git-credential-helper" {
		f := os.NewFile(3, "credential")
		if f == nil {
			os.Exit(125)
		}
		defer f.Close()
		if err := agenttask.CredentialHelper(os.Args[2], f, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "rubi-git: credential request refused")
			os.Exit(125)
		}
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "--bridge-relay" {
		if err := agentbridge.Relay(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "rubi-relay:", err)
			os.Exit(125)
		}
		return
	}
	config := flag.String("config", "", "trusted project binding JSON")
	flag.Parse()
	f, err := os.Open(*config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rubi-agent-exec: cannot open trusted configuration")
		os.Exit(125)
	}
	var binding agentsandbox.Binding
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	err = d.Decode(&binding)
	f.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "rubi-agent-exec: invalid trusted configuration")
		os.Exit(125)
	}
	launch, err := home.ResolveAgentLaunch(binding, *config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rubi-agent-exec:", err)
		os.Exit(125)
	}
	// Provider protocol arguments are appended individually, never shell-joined.
	launch.ConfigPath = *config
	launch.Args = append(launch.Args, flag.Args()...)
	ctx := context.Background()
	if binding.Task != "" {
		life, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		lease, e := (agenttask.Store{Home: binding.HomeDir}).Admit(life, binding.Task)
		if e != nil {
			fmt.Fprintln(os.Stderr, "rubi-agent-exec:", e)
			os.Exit(125)
		}
		// main uses os.Exit below, so close explicitly after the process is reaped.
		ctx = lease.Context()
		runTask(ctx, launch, binding, func() { lease.Close(); stop() })
		return
	}
	runTask(ctx, launch, binding, func() {})
}

func runTask(ctx context.Context, launch agentsandbox.Launch, binding agentsandbox.Binding, cleanup func()) {
	var code int
	var err error
	if binding.Provider == "opencode" && len(launch.Args) > 0 && launch.Args[0] == "serve" {
		code, err = serveOpenCode(ctx, launch)
	} else {
		code, err = agentsandbox.Run(ctx, launch, os.Stdin, os.Stdout, os.Stderr)
	}
	cleanup()
	if err != nil {
		fmt.Fprintln(os.Stderr, "rubi-agent-exec:", err)
		os.Exit(125)
	}
	os.Exit(code)
}
