// Command t3-bump moves Rubi's pin of T3 Code to the newest nightly (docs/plans/t3-integration.md S6): it writes
// t3/upstream.json, installs the pin into a temporary folder, runs the contract tests against it and writes a report of the
// input-schema changes of the tools Rubi uses. It changes files in the working tree only; a workflow opens the pull request.
//
//	go run ./cmd/t3-bump [-root .] [-report t3-bump-report.md] [-skip-contract]
//
// GITHUB_TOKEN, when set, is used for the GitHub API (it lifts the rate limit); it is sent to api.github.com only.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/Deikus-LXXVII/rubi/internal/t3"
	"github.com/Deikus-LXXVII/rubi/internal/t3bump"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, nil))
}

// run is main with its inputs given (tests pass a Runner and stand-in addresses). It returns the exit code: 0 for a bump
// or for "up to date", 1 for an error. A failed contract test is not an error: it is in the report, for the reviewer.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, runner t3bump.Runner) int {
	fs := flag.NewFlagSet("t3-bump", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "the repository root")
	report := fs.String("report", "t3-bump-report.md", "where to write the report")
	skip := fs.Bool("skip-contract", false, "write the pin only; no install, no contract tests")
	api := fs.String("github-api", t3bump.DefaultAPI, "the GitHub API (tests use a stand-in)")
	npm := fs.String("npm-registry", t3bump.DefaultNPM, "the npm registry (tests use a stand-in)")
	date := fs.String("date", t3bump.Today(), "the date to record in \"checked\"")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	abs, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	res, err := t3bump.Run(ctx, t3bump.Options{
		Root: abs, Client: &t3bump.Client{API: *api, NPM: *npm, Token: getenv("GITHUB_TOKEN")}, Runner: runner, Date: *date,
		SkipContract: *skip, UsedTools: t3.UsedTools,
	})
	if err != nil {
		fmt.Fprintln(stderr, "t3-bump:", err)
		return 1
	}
	text := t3bump.Report(res)
	if err := os.WriteFile(*report, []byte(text), 0o644); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if !res.Changed {
		fmt.Fprintf(stdout, "T3 is at the newest nightly (%s); nothing to do.\n", res.Old.Tag)
		return 0
	}
	fmt.Fprintf(stdout, "T3 pin: %s -> %s (install %s, contract %s, %d schema change(s)); report in %s\n",
		res.Old.Tag, res.New.Tag, res.Install, res.Contract, len(res.Changes), *report)
	return 0
}
