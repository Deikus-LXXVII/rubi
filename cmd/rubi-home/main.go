// Command rubi-home runs Rubi Home, the helper on a computer at home that lets Rubi control the Philips
// Hue Bridge and run the user's Shortcuts (see internal/home).
//
//	rubi-home run            serve Rubi's requests (the LaunchAgent runs this)
//	rubi-home pair           print a one-time code to paste into the Rubi panel
//	rubi-home status         show what is paired and allowed
//	rubi-home folder NAME    the Shortcuts folder Rubi may use (default: Rubi)
//	rubi-home unpair         forget every paired Rubi
//	rubi-home hue-forget ID  forget a Hue Bridge's recorded certificate (after replacing or resetting it)
//	rubi-home claude-hook    Claude Code's hooks: PermissionRequest asks Rubi and prints allow or deny;
//	                         SessionStart, UserPromptSubmit, Stop and SessionEnd report the session and
//	                         deliver messages waiting for it
//	rubi-home install-hook [--dry-run] [--to PATH] [--sudo] [--with-claude-plugin [--plugin-to PATH]]
//	                         copy this binary to the root-owned hook path (default /usr/local/bin/rubi-home)
//	                         with administrator rights, then verify it; --with-claude-plugin also installs the
//	                         embedded Claude Code plugin (the sister tools) to a root-owned directory
//	rubi-home claude-mcp     the stdio MCP server of the Claude Code plugin: rubi_sisters, rubi_ask, rubi_reply
//	                         and rubi_inbox_status, for the sister bound to the calling session (reads no config)
//	rubi-home claude-send (--session ID | --name TITLE) --from NAME [--wait-reply SECONDS]
//	                         queue the message on stdin for a Claude Code session (the documented way for a
//	                         local script to talk to one; reads no config); --wait-reply prints the answer;
//	                         rubi-home claude-send --get-reply MESSAGE_ID fetches an answer later
//	rubi-home version        print the version (reads nothing)
//	rubi-home claude-hook --wait [--wait-limit SECONDS]
//	                         a second, asyncRewake Stop hook: stays while the session is idle and wakes it
//	                         (exit 2, message on stderr) when a message arrives; gives up after the limit
//	                         (default 3570 seconds), always exiting 0 then
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home"
	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
	"github.com/Deikus-LXXVII/rubi/internal/relay"
	"github.com/Deikus-LXXVII/rubi/internal/version"
	"github.com/Deikus-LXXVII/rubi/internal/watch"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	if os.Args[1] == "claude-hook" && slices.Contains(os.Args[2:], "--wait") {
		// The idle-session waiter (a second Stop hook with asyncRewake): reads no config, prints nothing to
		// stdout, exits 2 only to wake the session with messages (written to stderr), 0 on every failure.
		os.Exit(runWait())
	}
	if os.Args[1] == "claude-hook" {
		// Only talks to the running helper; never opens or changes its files. Always prints a decision
		// and exits 0, so Claude Code reads the answer (a denial whenever anything goes wrong).
		if err := home.RunClaudeHook(os.Stdin, os.Stdout, home.ClaudeSocketPath(home.DefaultDir()), claudeHookLimit); err != nil {
			os.Exit(1) // couldn't even print: Claude Code's onFailure "block" denies
		}
		return
	}
	switch os.Args[1] {
	case "claude-mcp": // Claude Code's MCP server: talks to the helper on its socket, reads no configuration
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err := home.RunClaudeMCP(ctx, home.ClaudeSocketPath(home.DefaultDir()))
		stop()
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "rubi-home claude-mcp: %v\n", err)
			os.Exit(1)
		}
		return
	case "claude-send": // a local process's message for a session: talks to the helper on its socket, reads no configuration
		os.Exit(home.RunClaudeSend(os.Args[2:], os.Stdin, os.Stdout, os.Stderr, home.ClaudeSocketPath(home.DefaultDir())))
	case "version": // reads and creates nothing: the installer and status run it on any copy
		fmt.Println(version.Version)
		return
	case "install-hook": // needs no configuration either
		os.Exit(runInstallHook(os.Args[2:]))
	}
	h, err := home.Open(home.DefaultDir())
	if err != nil {
		log.Fatal(err)
	}
	switch os.Args[1] {
	case "run":
		run(h)
	case "pair":
		code, err := h.StartPairing()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("In the Rubi panel open Settings > Rubi Home > Add a home computer, and paste this code")
		fmt.Println("(it works once, within 15 minutes; Rubi Home must be running):")
		fmt.Println()
		fmt.Println(code.String())
	case "status":
		cfg := h.Config()
		fmt.Printf("Rubi Home %s on %q\n", version.Version, cfg.Name)
		sum := sha256.Sum256([]byte(h.ID.PublicBundle()))
		fmt.Printf("Fingerprint: %s\n", base64.RawURLEncoding.EncodeToString(sum[:6]))
		fmt.Printf("Paired with %d Rubi\n", len(cfg.Paired))
		for _, p := range cfg.Paired {
			fmt.Printf("  %s, since %s\n", p.Label, p.At.Local().Format("2006-01-02 15:04"))
		}
		fmt.Printf("Shortcuts folder: %s\n", cfg.ShortcutsFolder)
		fmt.Printf("Hue Bridges paired: %d\n", len(cfg.HuePins))
		home.InspectHookBinary(hookPath(os.Args[2:]), home.HookBinaryTimeout).WritePlain(os.Stdout, version.Version)
	case "folder":
		if len(os.Args) != 3 || strings.TrimSpace(os.Args[2]) == "" {
			usage()
		}
		if strings.EqualFold(strings.TrimSpace(os.Args[2]), "none") { // the Shortcuts CLI reads it as "no folder": every loose shortcut
			log.Fatal(`choose a real folder name, not "none"`)
		}
		if err := h.Update(func(c *home.Config) { c.ShortcutsFolder = os.Args[2] }); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Rubi may now use the shortcuts in the folder %q.\n", os.Args[2])
	case "unpair":
		if err := h.Update(func(c *home.Config) { c.Paired, c.Pending = nil, nil }); err != nil {
			log.Fatal(err)
		}
		fmt.Println("Forgot every paired Rubi. Pair again with: rubi-home pair")
	case "hue-forget":
		if len(os.Args) != 3 {
			usage()
		}
		if err := h.Update(func(c *home.Config) { delete(c.HuePins, strings.ToLower(os.Args[2])) }); err != nil {
			log.Fatal(err)
		}
		fmt.Println("Forgot that bridge. Set Hue up again in the Rubi panel.")
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: rubi-home run | pair | status | folder NAME | unpair | hue-forget ID | claude-hook [--wait [--wait-limit SECONDS]] | install-hook [--dry-run] [--to PATH] [--sudo] [--with-claude-plugin [--plugin-to PATH]] | claude-mcp | claude-send (--session ID | --name TITLE) --from NAME [--wait-reply SECONDS] | claude-send --get-reply ID | version\n       status [--hook PATH]: also checks the Claude Code hook binary (default "+home.DefaultHookBinary+", or $RUBI_HOOK_BINARY)")
	os.Exit(2)
}

// claudeHookLimit bounds a claude-hook's wait. The helper answers sooner (claude_wait_seconds, 280 by
// default); Claude Code's hook timeout (300 in the example settings) must be longer still.
const claudeHookLimit = 295 * time.Second

// runWait is the waiter; a panic anywhere in it exits 0 (only the deliberate 2 is non-zero).
func runWait() (code int) {
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()
	return home.RunClaudeWait(os.Stdin, os.Stderr, home.ClaudeSocketPath(home.DefaultDir()), waitLimit(os.Args[2:]))
}

// waitLimit is the waiter's --wait-limit in seconds (home.ClaudeWaitLimit when missing or not a positive
// number: a waiter never fails loudly, since exit 2 would wake the session).
func waitLimit(args []string) time.Duration {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--wait-limit" {
			if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 && n <= 86400 {
				return time.Duration(n) * time.Second
			}
		}
	}
	return home.ClaudeWaitLimit
}

func run(h *home.Helper) {
	key, err := h.RelayKey()
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Watching the Rubi uses this helper's own key: Rubi seals its registration to it.
	h.Watch = watch.New(h.ID.Box)
	go h.Watch.Run(ctx)
	// Claude Code's hook reaches the helper on a local socket (it can only ask; Rubi answers).
	go func() {
		if err := h.Claude.Serve(ctx, home.ClaudeSocketPath(h.Dir)); err != nil {
			log.Printf("Claude Code requests: %v", err)
		}
	}()
	h.Agent.Resume() // deliver what the agent queue still holds from an earlier run
	go func() {      // a sign-in container left by a crash
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		h.SweepAgent(ctx)
	}()
	defer h.Agent.Close()
	rpc := &homeproto.Server{Key: h.ID.Box, Handle: h.Handle}
	relays := h.Relays()
	last := -1
	srv := &relay.Server{Key: key, Relays: relays, Handle: rpc.HandleRPC, Logf: log.Printf,
		Ready: func(n int) {
			if (n == 0) != (last == 0) || last < 0 {
				log.Printf("relays connected: %d of %d", n, len(relays))
			}
			last = n
		}}
	go home.WarnIfHookOlder(home.HookBinaryPath(), version.Version, log.Printf) // best effort, off the hook paths
	log.Printf("Rubi Home %s running; Shortcuts folder %q", version.Version, h.Config().ShortcutsFolder)
	srv.Run(ctx)
}

// hookPath is status's --hook PATH, else $RUBI_HOOK_BINARY, else the default.
func hookPath(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--hook" {
			return args[i+1]
		}
	}
	return home.HookBinaryPath()
}

// runInstallHook is `rubi-home install-hook`; it returns the exit code.
func runInstallHook(args []string) int {
	o := home.InstallOptions{Dst: home.DefaultHookBinary, RequireRoot: true}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry-run":
			o.DryRun = true
		case "--sudo":
			o.Sudo = true
		case "--with-claude-plugin":
			o.WithPlugin = true
		case "--plugin-to":
			if i+1 >= len(args) {
				usage()
			}
			i++
			o.PluginDst = args[i]
		case "--to":
			if i+1 >= len(args) {
				usage()
			}
			i++
			o.Dst = args[i]
		default:
			usage()
		}
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "rubi-home: can't find this binary: %v\n", err)
		return 1
	}
	o.Src = exe
	if o.Sudo && !o.DryRun {
		if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			fmt.Fprintln(os.Stderr, "rubi-home: --sudo needs a terminal on stdin (sudo asks for a password); run it in a terminal, or leave out --sudo")
			return 1
		}
	}
	if err := home.InstallHook(context.Background(), os.Stdout, o); err != nil {
		fmt.Fprintf(os.Stderr, "rubi-home: %v\n", err)
		return 1
	}
	return 0
}
