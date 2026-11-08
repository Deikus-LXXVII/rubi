// rubi-agent-exec is the binaryPath adapter for a trusted, resolved session.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
)

func main() {
	config := flag.String("config", "", "trusted launch configuration JSON")
	flag.Parse()
	f, err := os.Open(*config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rubi-agent-exec: cannot open trusted configuration")
		os.Exit(125)
	}
	var launch agentsandbox.Launch
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	err = d.Decode(&launch)
	f.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "rubi-agent-exec: invalid trusted configuration")
		os.Exit(125)
	}
	// Provider protocol arguments are appended individually, never shell-joined.
	launch.ConfigPath = *config
	launch.Args = append(launch.Args, flag.Args()...)
	code, err := agentsandbox.Run(context.Background(), launch, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rubi-agent-exec:", err)
		os.Exit(125)
	}
	os.Exit(code)
}
