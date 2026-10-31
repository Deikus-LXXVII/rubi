// Package home is Rubi Home, the helper that runs on a computer at the user's home and does, on Rubi's
// request, what only a device on the home network can: talk to the Philips Hue Bridge, run the user's
// Shortcuts (which can control Apple Home), and pass Claude Code's permission requests on this computer to
// Rubi for approval. See internal/homeproto for how Rubi reaches it.
//
// It only does what it was built for: Hue requests are limited to the bridge's resource API, and only
// shortcuts in one folder (Rubi, by default) are visible and runnable. Everything else is refused.
package home

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
	"github.com/Deikus-LXXVII/rubi/internal/identity"
	"github.com/Deikus-LXXVII/rubi/internal/relay"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/internal/version"
	"github.com/Deikus-LXXVII/rubi/internal/watch"
)

// Config is kept in config.json in the helper's directory (0600).
type Config struct {
	Name     string    `json:"name"`
	RelayKey string    `json:"relay_key"` // hex; the routing key stays fixed so Rubi can find the helper
	Relays   []string  `json:"relays,omitempty"`
	Paired   []Pairing `json:"paired,omitempty"`
	Pending  *Pending  `json:"pending,omitempty"`
	// ShortcutsFolder is the only Shortcuts folder Rubi can see and run.
	ShortcutsFolder string `json:"shortcuts_folder"`
	// HuePins maps a bridge id to the SHA-256 of its certificate, recorded when it was paired.
	HuePins map[string]string `json:"hue_pins,omitempty"`
	// ClaudeWaitSeconds is how long a Claude Code request waits for Rubi (default 280; keep it below the
	// hook's timeout in Claude Code's settings).
	ClaudeWaitSeconds int `json:"claude_wait_seconds,omitempty"`
	// ClaudeStartDirs are the absolute directories (and everything inside them) where a Bot may have a new
	// Claude Code session started, besides the projects Claude Code already has sessions in. Default none.
	ClaudeStartDirs []string `json:"claude_start_dirs,omitempty"`
	// AgentProjects are the absolute directories an agent (the Agent SDK runner in a VM, capability
	// "claude-agent") may work in, one project each, mounted at /work. Default none. Encrypted per-project
	// volumes are a later milestone (agent-runner.md); for now these are plain directories.
	AgentProjects []string `json:"agent_projects,omitempty"`
	// AgentGitHosts are the git hosts the agent's proxy allows and the credential helper serves, for
	// example "git.example.com".
	AgentGitHosts []string `json:"agent_git_hosts,omitempty"`
	// AgentNetwork is the host-only network of Apple's container tool the VMs join ("rubi-agent" by
	// default). Home creates it with --internal when it is missing and refuses to start a VM on a network
	// whose mode isn't hostOnly.
	AgentNetwork string `json:"agent_network,omitempty"`
	// AgentProxyListen is where the agent proxy listens and AgentProxyVMHost the host name the VM uses to
	// reach it. By default the proxy listens on the Mac's address on AgentNetwork (its gateway, read from
	// the network when a VM starts, port chosen by the system) and the VM is told that address; set them only
	// to pin a port or to use another address.
	AgentProxyListen string `json:"agent_proxy_listen,omitempty"`
	AgentProxyVMHost string `json:"agent_proxy_vm_host,omitempty"`
	// AgentRunnerDir is the runner/ directory the VM image is built from when it isn't built yet.
	AgentRunnerDir string `json:"agent_runner_dir,omitempty"`
	// AgentAllowedHosts are extra hosts (port 443) the agents' proxy allows for every project, for example
	// "packages.example.net" for a Swift package; nothing is read from a project's own files. Hosts not
	// listed are asked about (net.ask) and a plugin approval is remembered per project.
	AgentAllowedHosts []string `json:"agent_allowed_hosts,omitempty"`
	// AgentNoDefaultForges drops github.com, gitlab.com and bitbucket.org from the default allow-list.
	AgentNoDefaultForges bool `json:"agent_no_default_forges,omitempty"`
	// AgentAllowBypass says whether agent sessions may run in bypassPermissions or dontAsk (default true;
	// the plugin still decides every tool call and approves loosening a mode with a passkey).
	AgentAllowBypass *bool `json:"agent_allow_bypass,omitempty"`
	// AgentDefaultMode ("maximum", "medium" (default) or "light") is the usage mode of a Bot met for the
	// first time and of the sessions the user starts from the panel; AgentBotModes has the mode of each Bot
	// (by the Bot id the plugin passes). Presets for how much a Bot may run at once: see agent/usage.go. The
	// user changes modes from the plugin's panel; a Bot can't.
	AgentDefaultMode string            `json:"agent_default_mode,omitempty"`
	AgentBotModes    map[string]string `json:"agent_bot_modes,omitempty"`
	// AgentMaxSessionsPerProject is how many top-level agent sessions may run at once in one project
	// (default 1, at least 1); more wait in the queue. Subagents and forks inside a session don't count.
	AgentMaxSessionsPerProject int `json:"agent_max_sessions_per_project,omitempty"`
	// AgentMaxSessions caps top-level agent sessions running at once on the computer, whatever the Bots'
	// modes allow (0 = no cap; default 4: a Bot on maximum is bounded only by this).
	AgentMaxSessions *int `json:"agent_max_sessions,omitempty"`
	// AgentMaxSubagentsPerSession caps subagents running at once in one session for every Bot (0 = no cap;
	// unset: each Bot's mode decides). A session over it waits; nothing is denied.
	AgentMaxSubagentsPerSession *int `json:"agent_max_subagents_per_session,omitempty"`
}

// Pairing is one Rubi allowed to use the helper; only a hash of its token is kept.
type Pairing struct {
	TokenHash string    `json:"token_hash"`
	Label     string    `json:"label,omitempty"`
	At        time.Time `json:"at"`
}

type Pending struct {
	Secret  string    `json:"secret"`
	Expires time.Time `json:"expires"`
}

const pairingTTL = 15 * time.Minute

// Helper is a running Rubi Home.
type Helper struct {
	Dir string
	ID  *identity.Identity
	// Shortcuts runs the shortcuts command (a seam for tests).
	Shortcuts func(ctx context.Context, args ...string) ([]byte, error)
	Hue       *Hue
	// Watch, when set, watches the paired Rubi and wakes its administrator when it goes down or stays locked.
	Watch *watch.Watcher
	// Claude holds Claude Code's permission requests for Rubi (see claude.go).
	Claude *Claude
	// Agent runs the Agent SDK runner in a VM per project (see agent/ and claude-agent.* ops).
	Agent *agent.Manager

	mu  sync.Mutex
	cfg Config
}

// DefaultDir is ~/Library/Application Support/Rubi Home on a Mac (the user config dir elsewhere).
func DefaultDir() string {
	if d := os.Getenv("RUBI_HOME_DIR"); d != "" {
		return d
	}
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	return filepath.Join(base, "Rubi Home")
}

// Open loads (or creates) the helper's identity and configuration.
func Open(dir string) (*Helper, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	id, err := identity.LoadOrCreate(filepath.Join(dir, "identity.json"))
	if err != nil {
		return nil, err
	}
	h := &Helper{Dir: dir, ID: id, Shortcuts: runShortcuts}
	if err := h.load(); err != nil {
		return nil, err
	}
	h.Hue = &Hue{Pins: h.huePins, SetPin: h.setHuePin}
	h.Claude = &Claude{}
	h.Claude.DesktopStore, h.Claude.Transcripts = DefaultClaudeDirs()
	h.Claude.QueueFile = filepath.Join(dir, ClaudeQueueFile)
	h.Claude.Launcher = newDefaultLauncher()
	h.Claude.StartDirs = func() []string { return h.Fresh().ClaudeStartDirs }
	h.Claude.ProtectedDirs = []string{dir}
	h.Claude.loadQueue()
	h.Agent = h.newAgent()
	if s := h.Config().ClaudeWaitSeconds; s >= 10 && s <= 590 {
		h.Claude.Wait = time.Duration(s) * time.Second
	}
	return h, nil
}

// AgentCredFile is the git credential file for agents in the helper's directory (see agent.FileCredSource).
const AgentCredFile = "agent-git-credentials.json"

// newAgent makes the agent manager from the configuration (read at each use, so the CLI's changes apply).
func (h *Helper) newAgent() *agent.Manager {
	home, _ := os.UserHomeDir()
	return agent.New(agent.Options{
		Dir:       h.Dir,
		Protected: []string{home},
		Projects:  func() []string { return h.Fresh().AgentProjects },
		GitHosts:  func() []string { return h.Fresh().AgentGitHosts },
		NewVM: func(p agent.Project) agent.VM {
			return &agent.AppleContainer{RunnerDir: h.Fresh().AgentRunnerDir, Network: h.Fresh().AgentNetwork, EnvDir: h.Dir}
		},
		AllowedHosts:    func() []string { return h.Fresh().AgentAllowedHosts },
		NoDefaultForges: func() bool { return h.Fresh().AgentNoDefaultForges },
		AllowBypass: func() bool {
			b := h.Fresh().AgentAllowBypass
			return b == nil || *b
		},
		Usage:    func() agent.UsageConfig { return h.Fresh().agentUsage() },
		SetUsage: func(ch agent.UsageChange) error { return h.Update(func(c *Config) { c.applyUsageChange(ch) }) },
		SeeBot: func(id string) {
			_ = h.Update(func(c *Config) {
				if _, ok := c.AgentBotModes[id]; !ok && len(c.AgentBotModes) < agent.MaxBots {
					c.setBotMode(id, agent.UsageConfig{Default: c.AgentDefaultMode}.ModeOf(""))
				}
			})
		},
		Cred:        agent.FileCredSource{Path: filepath.Join(h.Dir, AgentCredFile)},
		ProxyListen: h.Fresh().AgentProxyListen,
		ProxyVMHost: h.Fresh().AgentProxyVMHost,
	})
}

func (c Config) agentUsage() agent.UsageConfig {
	return agent.UsageConfig{Default: c.AgentDefaultMode, Bots: c.AgentBotModes, MaxSessionsPerProject: c.AgentMaxSessionsPerProject,
		MaxSessions: c.AgentMaxSessions, MaxSubagents: c.AgentMaxSubagentsPerSession}
}

func (c *Config) setBotMode(id, mode string) {
	if c.AgentBotModes == nil {
		c.AgentBotModes = map[string]string{}
	}
	c.AgentBotModes[id] = mode
}

// applyUsageChange sets the mode of one Bot, of the default, or of every known Bot and the default.
func (c *Config) applyUsageChange(ch agent.UsageChange) {
	switch {
	case ch.All:
		c.AgentDefaultMode = ch.Mode
		for id := range c.AgentBotModes {
			c.AgentBotModes[id] = ch.Mode
		}
	case ch.Default:
		c.AgentDefaultMode = ch.Mode
	case ch.Bot != "" && (len(c.AgentBotModes) < agent.MaxBots || c.AgentBotModes[ch.Bot] != ""):
		c.setBotMode(ch.Bot, ch.Mode)
	}
}

func (h *Helper) path() string { return filepath.Join(h.Dir, "config.json") }

func (h *Helper) load() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.loadLocked()
}

func (h *Helper) loadLocked() error {
	cfg := Config{}
	b, err := os.ReadFile(h.path())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return fmt.Errorf("config.json: %w", err)
		}
	}
	changed := false
	if cfg.RelayKey == "" {
		k, err := relay.NewKey()
		if err != nil {
			return err
		}
		cfg.RelayKey, changed = k.Hex(), true
	}
	if cfg.ShortcutsFolder == "" {
		cfg.ShortcutsFolder, changed = "Rubi", true
	}
	if cfg.Name == "" {
		n, _ := os.Hostname()
		cfg.Name, changed = computerName(n), true
	}
	h.cfg = cfg
	if changed {
		return h.saveLocked()
	}
	return nil
}

func (h *Helper) saveLocked() error {
	b, err := json.MarshalIndent(h.cfg, "", "  ")
	if err != nil {
		return err
	}
	return vault.WriteFileAtomic(h.path(), b, 0o600)
}

// Fresh returns the configuration as it is on disk now (the CLI may have changed it).
func (h *Helper) Fresh() Config {
	_ = h.load()
	return h.Config()
}

// Config returns a copy of the configuration.
func (h *Helper) Config() Config {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg
}

// Update changes the configuration and saves it.
func (h *Helper) Update(fn func(*Config)) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.loadLocked(); err != nil {
		return err
	}
	fn(&h.cfg)
	return h.saveLocked()
}

// RelayKey is the helper's fixed routing key.
func (h *Helper) RelayKey() (*relay.Key, error) { return relay.KeyFromHex(h.Config().RelayKey) }

// Relays the helper listens on.
func (h *Helper) Relays() []string {
	if r := h.Config().Relays; len(r) > 0 {
		return r
	}
	return homeproto.DefaultRelays()
}

// StartPairing makes a new one-time pairing code (valid 15 minutes).
func (h *Helper) StartPairing() (homeproto.Code, error) {
	secret := homeproto.RandomToken()
	if err := h.Update(func(c *Config) { c.Pending = &Pending{Secret: secret, Expires: time.Now().Add(pairingTTL)} }); err != nil {
		return homeproto.Code{}, err
	}
	key, err := h.RelayKey()
	if err != nil {
		return homeproto.Code{}, err
	}
	cfg := h.Config()
	return homeproto.Code{Relay: key.Public(), Bundle: h.ID.PublicBundle(), Relays: cfg.Relays, Secret: secret, Name: cfg.Name}, nil
}

func tokenHash(t string) string {
	sum := sha256.Sum256([]byte("rubi-home-token|" + t))
	return hex.EncodeToString(sum[:])
}

func (h *Helper) paired(token string) bool {
	if token == "" {
		return false
	}
	_ = h.load() // pick up `rubi-home unpair` run from another process
	want := tokenHash(token)
	ok := false
	for _, p := range h.Config().Paired {
		ok = ok || subtle.ConstantTimeCompare([]byte(p.TokenHash), []byte(want)) == 1
	}
	return ok
}

// Handle serves one request from Rubi.
func (h *Helper) Handle(ctx context.Context, env homeproto.Envelope) (any, error) {
	if env.Op == "pair" {
		return h.pair(env.Args)
	}
	if env.Op == "watch.beat" && h.Watch != nil {
		// Signed by Rubi's watch key and carrying a registration sealed to this helper: no token needed,
		// so a locked Rubi (which can't read its pairing) can still say it is alive.
		var m watch.Message
		if json.Unmarshal(env.Args, &m) != nil {
			return nil, errors.New("bad arguments")
		}
		return nil, h.Watch.Receive(m)
	}
	if !h.paired(env.Token) {
		return nil, errors.New(homeproto.NotPairedMessage)
	}
	switch env.Op {
	case "hello":
		return h.hello(), nil
	case "unpair":
		want := tokenHash(env.Token)
		return nil, h.Update(func(c *Config) {
			kept := c.Paired[:0]
			for _, p := range c.Paired {
				if p.TokenHash != want {
					kept = append(kept, p)
				}
			}
			c.Paired = kept
		})
	case "shortcuts.list":
		return h.listShortcuts(ctx)
	case "shortcuts.run":
		var in struct {
			Name  string `json:"name"`
			Input string `json:"input"`
		}
		if err := json.Unmarshal(env.Args, &in); err != nil {
			return nil, errors.New("bad arguments")
		}
		return h.runShortcut(ctx, in.Name, in.Input)
	case "hue.discover":
		return h.Hue.Discover(ctx)
	case "hue.pair":
		var in struct {
			IP string `json:"ip"`
		}
		if err := json.Unmarshal(env.Args, &in); err != nil {
			return nil, errors.New("bad arguments")
		}
		return h.Hue.Pair(ctx, in.IP)
	case "hue.request":
		var in HueRequest
		if err := json.Unmarshal(env.Args, &in); err != nil {
			return nil, errors.New("bad arguments")
		}
		return h.Hue.Do(ctx, in)
	case "claude.next", "claude.decide", "claude-sessions.list", "claude-sessions.send", "claude-sessions.next", "claude-sessions.bound", "claude-start.create":
		return h.Claude.handle(ctx, env.Op, env.Args)
	case "claude-agent.projects", "claude-agent.sessions", "claude-agent.messages", "claude-agent.start", "claude-agent.send",
		"claude-agent.rename", "claude-agent.fork", "claude-agent.interrupt", "claude-agent.close", "claude-agent.mode",
		"claude-agent.next", "claude-agent.decide", "claude-agent.status", "claude-agent.usage_mode":
		return h.Agent.Handle(ctx, env.Op, env.Args)
	}
	return nil, fmt.Errorf("Rubi Home doesn't do %q", env.Op)
}

func (h *Helper) hello() map[string]any {
	cfg := h.Config()
	return map[string]any{"name": cfg.Name, "version": version.Version, "shortcuts_folder": cfg.ShortcutsFolder,
		"can": []string{"hue", "shortcuts", "claude", "claude-sessions", "claude-start", "claude-agent"}}
}

func (h *Helper) pair(args json.RawMessage) (any, error) {
	var in struct {
		Secret string `json:"secret"`
		Token  string `json:"token"`
		Label  string `json:"label"`
	}
	if err := json.Unmarshal(args, &in); err != nil || len(in.Token) < 32 || len(in.Token) > 256 {
		return nil, errors.New("bad arguments")
	}
	if r := []rune(in.Label); len(r) > 64 {
		in.Label = string(r[:64])
	}
	expired := errors.New("this pairing code has expired or was already used; run `rubi-home pair` again")
	if p := h.Fresh().Pending; p == nil || time.Now().After(p.Expires) {
		return nil, expired // nothing to change: no write to disk for a stranger's guess
	}
	var err error
	uerr := h.Update(func(c *Config) {
		p := c.Pending
		if p == nil || time.Now().After(p.Expires) || subtle.ConstantTimeCompare([]byte(p.Secret), []byte(in.Secret)) != 1 {
			err = expired
			return
		}
		c.Pending = nil
		// A new pairing replaces every earlier one. Codes are made on this computer by its owner, so a new
		// one means "this Rubi, from now on": a pairing someone got from an earlier code ends here.
		c.Paired = []Pairing{{TokenHash: tokenHash(in.Token), Label: in.Label, At: time.Now().UTC()}}
	})
	if uerr != nil {
		return nil, uerr
	}
	if err != nil {
		return nil, err
	}
	return h.hello(), nil
}

func (h *Helper) huePins() map[string]string { return h.Fresh().HuePins }

func (h *Helper) setHuePin(bridge, pin string) error {
	return h.Update(func(c *Config) {
		if c.HuePins == nil {
			c.HuePins = map[string]string{}
		}
		c.HuePins[bridge] = pin
	})
}

// computerName is how this computer introduces itself to Rubi (and through it to plugins and the
// agent): the hostname without ".local" and without an owner's name ("Annas-MacBook-Pro" -> "MacBook Pro").
func computerName(host string) string {
	n := strings.TrimSpace(strings.ReplaceAll(strings.TrimSuffix(strings.TrimSpace(host), ".local"), "-", " "))
	if m := macModel.FindString(n); m != "" {
		return m
	}
	if i := strings.Index(strings.ToLower(n), "'s "); i >= 0 {
		n = strings.TrimSpace(n[i+3:])
	} else if i := strings.Index(n, "’s "); i >= 0 {
		n = strings.TrimSpace(n[i+len("’s "):])
	}
	if n == "" {
		return "Home computer"
	}
	return n
}

var macModel = regexp.MustCompile(`(?i)(MacBook( (Air|Pro))?|Mac mini|Mac Studio|Mac Pro|iMac)( \d+)?$`)
