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
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/t3vm"
	"github.com/Deikus-LXXVII/rubi/internal/home/hostpaths"
	"github.com/Deikus-LXXVII/rubi/internal/home/setup"
	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
	"github.com/Deikus-LXXVII/rubi/internal/identity"
	"github.com/Deikus-LXXVII/rubi/internal/library/libsig"
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
	// AgentNetwork, AgentProxyListen and AgentProxyVMHost are obsolete and ignored: VMs run with no network at all
	// (`container run --network none`) and reach the proxy through a stream pipe to Rubi Home
	// (docs/design/agent-runner.md, "No network: rubi-netpipe"), so there is no host-only network, no listener on the
	// Mac and no address to choose. They are still read so that an old configuration file parses.
	AgentNetwork     string `json:"agent_network,omitempty"`
	AgentProxyListen string `json:"agent_proxy_listen,omitempty"`
	AgentProxyVMHost string `json:"agent_proxy_vm_host,omitempty"`
	// T3Port and T3BaseDir describe the T3 server Rubi Home runs (docs/plans/t3-integration.md): its port on
	// loopback and its data folder. While T3Port is 0 Home serves no T3 sessions (no agent-vm.sock) and writes no
	// role instances.
	T3Port    int    `json:"t3_port,omitempty"`
	T3BaseDir string `json:"t3_base_dir,omitempty"`
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
	// AgentMaxSessionsPerProfile is how many top-level agent sessions one role (a profile of a project; the
	// project's legacy role is one) may run at once (default 1, at least 1); more wait in the queue. Subagents and
	// forks inside a session don't count. AgentMaxSessionsPerProject is an optional extra cap on one project's
	// roles together (unset or 0: none). The user changes both from the plugin's panel (claude-agent.limits).
	AgentMaxSessionsPerProfile int `json:"agent_max_sessions_per_profile,omitempty"`
	AgentMaxSessionsPerProject int `json:"agent_max_sessions_per_project,omitempty"`
	// AgentMaxSessions caps top-level agent sessions running at once on the computer, whatever the Bots'
	// modes allow (0 = no cap; default 4: a Bot on maximum is bounded only by this).
	AgentMaxSessions *int `json:"agent_max_sessions,omitempty"`
	// AgentRoleVMIdleMinutes is how long an isolated role's VM may run with no session and nothing queued before
	// Home stops it (default 15; it starts again on the next use). The project's legacy-role VM is not stopped.
	AgentRoleVMIdleMinutes int `json:"agent_role_vm_idle_minutes,omitempty"`
	// AgentMaxSubagentsPerSession caps subagents running at once in one session for every Bot (0 = no cap;
	// unset: each Bot's mode decides). A session over it waits; nothing is denied.
	AgentMaxSubagentsPerSession *int `json:"agent_max_subagents_per_session,omitempty"`
	// LibraryKey is the public key (base64) that signs the skills in Rubi's shared library, pinned the first time
	// Rubi presents it (op library.key, which only Rubi's core sends; a plugin's home.call can't reach it). A
	// different key is refused, and a new pairing forgets it.
	LibraryKey string `json:"library_key,omitempty"`
	// LibraryRevoked is the newest signed list of withdrawn library versions ("id@version") Rubi's core pushed (op
	// library.revoked, core only, signed with the library key), and LibraryRevokedAt when it was issued. Home
	// builds no role with these and takes them out of the ones it has built.
	LibraryRevoked   []string `json:"library_revoked,omitempty"`
	LibraryRevokedAt int64    `json:"library_revoked_at,omitempty"`
}

// Pairing is one Rubi allowed to use the helper; only a hash of its token is kept.
type Pairing struct {
	TokenHash string    `json:"token_hash"`
	Label     string    `json:"label,omitempty"`
	At        time.Time `json:"at"`
}

type Pending struct {
	Secret   string    `json:"secret"`
	Pin      string    `json:"pin,omitempty"` // the short secret of a link pairing, as typed (no dash)
	Expires  time.Time `json:"expires"`
	Attempts int       `json:"attempts,omitempty"` // wrong secrets so far
}

const pairingTTL = 15 * time.Minute

// maxPairAttempts is how many wrong secrets void a pending code: a short PIN can't be guessed in five tries.
const maxPairAttempts = 5

// Helper is a running Rubi Home.
type Helper struct {
	Dir string
	ID  *identity.Identity
	// vmOverride, when set, makes the agents' VM instead of the Apple container (a seam for tests).
	vmOverride func(agent.Project) agent.VM
	// Shortcuts runs the shortcuts command (a seam for tests).
	Shortcuts func(ctx context.Context, args ...string) ([]byte, error)
	Hue       *Hue
	// Watch, when set, watches the paired Rubi and wakes its administrator when it goes down or stays locked.
	Watch *watch.Watcher
	// T3Pid, when set, gives the process id of the T3 server Home runs (0 while it isn't running): T3 sessions are
	// then only served to processes below it. Unset, the session socket is served to the same user.
	T3Pid func() int
	// Claude holds Claude Code's permission requests for Rubi (see claude.go).
	Claude *Claude
	// Agent runs the Agent SDK runner in a VM per project (see agent/ and claude-agent.* ops).
	Agent *agent.Manager
	// Setup is the button-only setup of the agent path (see setup/ and the agent-setup.* ops).
	Setup *setup.Setup
	// T3 is the T3 server Home installs and runs, and the t3-agent.* and t3-setup.* ops on it (see t3home.go).
	T3 *T3Home

	// hook state and installation (hookstate.go); the lower-case fields are seams for tests.
	hookMu    sync.Mutex  // guards hookBusy, hookLast and hookSeen
	hookBusy  bool        // an installation is running
	hookLast  *hookResult // how the last installation ended, until the next one starts
	hookSeen  hookCached  // the last inspection of the hook file
	hookPath  string      // the hook binary's path ("" is DefaultHookBinary)
	pluginAt  string      // the Claude Code plugin's directory ("" is DefaultClaudePluginDir)
	hookOpts  func(*InstallOptions)
	hookTweak func(*HookBinary) // changes what the inspection found (a test cannot make a root-owned file)
	hookLimit time.Duration     // how long an installation may take (hookInstallLimit when zero)
	hookClock func() time.Time  // the time the cooldown and the cache use (time.Now when nil)

	mu  sync.Mutex
	cfg Config

	sessMu sync.Mutex
	sess   *t3vm.Server // the T3 session server while it serves (see ServeAgentVM)
}

// DefaultDir is Rubi Home's folder: "Rubi Home" in hostpaths.DataBase (~/Library/Application Support on a Mac, the XDG
// config folder on Linux, %LOCALAPPDATA% on Windows). It fails
// when there is no config dir (it never falls back to the working directory).
func DefaultDir() (string, error) {
	if d := os.Getenv("RUBI_HOME_DIR"); d != "" {
		return d, nil
	}
	base, err := hostpaths.DataBase()
	if err != nil || base == "" {
		return "", errors.New("can't find a configuration folder for Rubi Home; set RUBI_HOME_DIR")
	}
	return filepath.Join(base, "Rubi Home"), nil
}

// OpenExisting is Open for read-only commands: it creates nothing, and fails when Rubi Home was never set up.
func OpenExisting(dir string) (*Helper, error) {
	if _, err := os.Stat(filepath.Join(dir, "identity.json")); err != nil {
		return nil, fmt.Errorf("Rubi Home isn't set up in %q yet (open Rubi Home on that computer)", dir)
	}
	return Open(dir)
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
	h.Hue = &Hue{Pins: h.huePins, SetPin: h.setHuePin, Unpin: h.unsetHuePin}
	h.Claude = &Claude{}
	h.Claude.DesktopStore, h.Claude.Transcripts = DefaultClaudeDirs()
	h.Claude.QueueFile = filepath.Join(dir, ClaudeQueueFile)
	h.Claude.Launcher = newDefaultLauncher()
	h.Claude.StartDirs = func() []string { return h.Fresh().ClaudeStartDirs }
	h.Claude.ProtectedDirs = []string{dir}
	h.Claude.loadQueue()
	h.Agent = h.newAgent()
	h.Setup = h.newSetup()
	h.T3 = newT3Home(h)
	h.T3Pid = h.T3.Pid
	if s := h.Config().ClaudeWaitSeconds; s >= 10 && s <= 590 {
		h.Claude.Wait = time.Duration(s) * time.Second
	}
	return h, nil
}

// legacyAuthDirName is the folder the earlier design kept its long-lived token in. That design is gone (the credential is
// claude's own, in the sign-in folder); the old folder is removed at Home's start so no stale token stays on disk. Only the
// folder is named here, nothing in it is opened.
const legacyAuthDirName = "agent-auth"

// removeLegacyAuthDir deletes the old token folder if it is a plain folder of Home's directory.
func (h *Helper) removeLegacyAuthDir() {
	dir := filepath.Join(h.Dir, legacyAuthDirName)
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("agent: removing the old sign-in folder %s: %v", legacyAuthDirName, err)
		return
	}
	log.Printf("agent: removed the old sign-in folder %s (the earlier design's token); sign in again for T3 Agents", legacyAuthDirName)
}

// sweepSignIn removes the sign-in containers of both paths (a variable so that a test can see that SweepAgent calls it).
var sweepSignIn = (*agent.AppleContainer).Sweep

// SweepAgent removes what a crash of an earlier run can have left in the container tool: the sign-in
// container. It is for Rubi Home's start (not for tests, which never run the real tool).
func (h *Helper) SweepAgent(ctx context.Context) {
	h.removeLegacyAuthDir()
	c := h.Fresh()
	ac := &agent.AppleContainer{RunnerDir: c.AgentRunnerDir, EnvDir: h.Dir}
	sweepSignIn(ac, ctx)
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
			if h.vmOverride != nil {
				return h.vmOverride(p)
			}
			c := h.Fresh()
			return &agent.AppleContainer{RunnerDir: c.AgentRunnerDir, EnvDir: h.Dir}
		},
		AllowedHosts:    func() []string { return h.Fresh().AgentAllowedHosts },
		NoDefaultForges: func() bool { return h.Fresh().AgentNoDefaultForges },
		AllowBypass: func() bool {
			b := h.Fresh().AgentAllowBypass
			return b == nil || *b
		},
		RoleVMIdle: func() time.Duration { return time.Duration(h.Fresh().AgentRoleVMIdleMinutes) * time.Minute },
		Usage:      func() agent.UsageConfig { return h.Fresh().agentUsage() },
		SetUsage:   func(ch agent.UsageChange) error { return h.Update(func(c *Config) { c.applyUsageChange(ch) }) },
		SetLimits: func(ch agent.LimitsChange) error {
			return h.Update(func(c *Config) { c.applyLimitsChange(ch) })
		},
		SeeBot: func(id string) {
			_ = h.Update(func(c *Config) {
				if _, ok := c.AgentBotModes[id]; !ok && len(c.AgentBotModes) < agent.MaxBots {
					c.setBotMode(id, agent.UsageConfig{Default: c.AgentDefaultMode}.ModeOf(""))
				}
			})
		},
		RoleVerifier: roles.Pinned{Key: h.libraryKey, IsRevoked: h.libraryRevoked},
		Cred:         agent.FileCredSource{Path: filepath.Join(h.Dir, AgentCredFile)},
		ProxyListen:  h.Fresh().AgentProxyListen,
		ProxyVMHost:  h.Fresh().AgentProxyVMHost,
	})
}

// newSetup makes the agent setup; it reads and changes the agent_* configuration like the CLI does.
func (h *Helper) newSetup() *setup.Setup {
	return setup.New(setup.Options{
		Dir: h.Dir,
		Agents: setup.ManagerAgents{M: h.Agent, SignInDir: func() string { return agent.SignInDirOf(h.Dir) },
			Auth: func() *agent.AppleContainer {
				c := h.Fresh()
				return &agent.AppleContainer{RunnerDir: c.AgentRunnerDir, EnvDir: h.Dir}
			}},
		Settings: func() setup.Settings {
			c := h.Fresh()
			return setup.Settings{Projects: c.AgentProjects, ProxyListen: c.AgentProxyListen,
				ProxyVMHost: c.AgentProxyVMHost, RunnerDir: c.AgentRunnerDir}
		},
		Update: func(fn func(*setup.Settings)) error {
			return h.Update(func(c *Config) {
				st := setup.Settings{Projects: c.AgentProjects, ProxyListen: c.AgentProxyListen,
					ProxyVMHost: c.AgentProxyVMHost, RunnerDir: c.AgentRunnerDir}
				fn(&st)
				c.AgentProjects, c.AgentProxyListen, c.AgentProxyVMHost = st.Projects, st.ProxyListen, st.ProxyVMHost
			})
		},
	})
}

func (c Config) agentUsage() agent.UsageConfig {
	return agent.UsageConfig{Default: c.AgentDefaultMode, Bots: c.AgentBotModes, MaxSessionsPerProfile: c.AgentMaxSessionsPerProfile, MaxSessionsPerProject: c.AgentMaxSessionsPerProject,
		MaxSessions: c.AgentMaxSessions, MaxSubagents: c.AgentMaxSubagentsPerSession}
}

// applyLimitsChange sets the session limits that are given.
func (c *Config) applyLimitsChange(ch agent.LimitsChange) {
	if ch.PerProfile != nil {
		c.AgentMaxSessionsPerProfile = *ch.PerProfile
	}
	if ch.PerProject != nil {
		c.AgentMaxSessionsPerProject = *ch.PerProject
	}
	if ch.Total != nil {
		t := *ch.Total
		c.AgentMaxSessions = &t
	}
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
	// The read-modify-write is also one step for other processes (the CLI and the running helper): without
	// the file lock the later writer would drop the other's change.
	unlock, err := lockFile(h.path() + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
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
	code, _, err := h.PairingCode(false)
	return code, err
}

// PairingCode returns the full pairing code and its PIN. With reuse, a pending code that is still good (not
// expired, not worn out by wrong tries, at least a minute left) is returned as it is instead of being replaced.
func (h *Helper) PairingCode(reuse bool) (homeproto.Code, string, error) {
	secret, pin := homeproto.RandomToken(), homeproto.NewPIN()
	if err := h.Update(func(c *Config) {
		if p := c.Pending; reuse && p != nil && p.Pin != "" && p.Attempts < maxPairAttempts && time.Until(p.Expires) > time.Minute {
			secret, pin = p.Secret, p.Pin
			return
		}
		c.Pending = &Pending{Secret: secret, Pin: pin, Expires: time.Now().Add(pairingTTL)}
	}); err != nil {
		return homeproto.Code{}, "", err
	}
	key, err := h.RelayKey()
	if err != nil {
		return homeproto.Code{}, "", err
	}
	cfg := h.Config()
	return homeproto.Code{Relay: key.Public(), Bundle: h.ID.PublicBundle(), Relays: cfg.Relays, Secret: secret, Name: cfg.Name}, pin, nil
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
	case "hook.plan":
		return h.planHook(env.Args)
	case "hook.install":
		return h.installHook(env.Args)
	case "library.key":
		return h.pinLibraryKey(env.Args)
	case "library.revoked":
		return h.setLibraryRevoked(env.Args)
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
	case "hue.forget":
		var in struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(env.Args, &in); err != nil {
			return nil, errors.New("bad arguments")
		}
		return h.Hue.Forget(in.ID)
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
		"claude-agent.next", "claude-agent.decide", "claude-agent.status", "claude-agent.usage_mode", "claude-agent.limits",
		"claude-agent.role.list", "claude-agent.role.get", "claude-agent.role.history", "claude-agent.role.diff", "claude-agent.role.put",
		"claude-agent.ssh.scan", "claude-agent.ssh.register", "claude-agent.ssh.list", "claude-agent.ssh.remove", "claude-agent.ssh.audit":
		return h.Agent.Handle(ctx, env.Op, env.Args)
	case "t3-agent.projects", "t3-agent.sessions", "t3-agent.messages", "t3-agent.start", "t3-agent.send",
		"t3-agent.rename", "t3-agent.fork", "t3-agent.interrupt", "t3-agent.close", "t3-agent.mode",
		"t3-agent.next", "t3-agent.decide", "t3-agent.status", "t3-agent.usage_mode", "t3-agent.limits",
		"t3-agent.role.list", "t3-agent.role.get", "t3-agent.role.history", "t3-agent.role.diff", "t3-agent.role.put",
		"t3-agent.ssh.scan", "t3-agent.ssh.register", "t3-agent.ssh.list", "t3-agent.ssh.remove", "t3-agent.ssh.audit":
		return h.T3.handleAgent(ctx, env.Op, env.Args)
	case "t3-setup.status", "t3-setup.run", "t3-setup.login.code", "t3-setup.login.open", "t3-setup.project.add",
		"t3-setup.project.remove", "t3-setup.cancel":
		return h.T3.handleSetup(ctx, env.Op, env.Args)
	case "agent-setup.status", "agent-setup.run", "agent-setup.login.code", "agent-setup.login.open",
		"agent-setup.project.add", "agent-setup.project.remove", "agent-setup.cancel":
		return h.Setup.Handle(ctx, env.Op, env.Args)
	}
	return nil, fmt.Errorf("Rubi Home doesn't do %q", env.Op)
}

// libraryKey is the pinned library public key (nil: none).
func (h *Helper) libraryKey() ed25519.PublicKey {
	b, err := base64.StdEncoding.DecodeString(h.Fresh().LibraryKey)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(b)
}

// pinLibraryKey pins the key Rubi's core presents (the first one wins; a different one is refused).
func (h *Helper) pinLibraryKey(args json.RawMessage) (any, error) {
	var in struct {
		PublicKey string `json:"public_key"`
	}
	if json.Unmarshal(args, &in) != nil {
		return nil, errors.New("bad arguments")
	}
	b, err := base64.StdEncoding.DecodeString(in.PublicKey)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("bad arguments")
	}
	var refused error
	uerr := h.Update(func(c *Config) {
		switch {
		case c.LibraryKey == "":
			c.LibraryKey = in.PublicKey
		case c.LibraryKey != in.PublicKey:
			refused = errors.New("a different library key is already pinned on this computer; pair with Rubi again to change it")
		}
	})
	if uerr != nil {
		return nil, uerr
	}
	if refused != nil {
		return nil, refused
	}
	return map[string]any{"pinned": true}, nil
}

// libraryRevoked says whether the signed list from Rubi's core names this library version.
func (h *Helper) libraryRevoked(id, version string) bool {
	want := id + "@" + version
	for _, r := range h.Fresh().LibraryRevoked {
		if r == want {
			return true
		}
	}
	return false
}

var revokedRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}@[1-9][0-9]{0,5}$`)

const maxRevoked = 5000

// setLibraryRevoked takes the list of withdrawn library versions Rubi's core signed with the library key. Only the
// newest list counts (an older one is refused, so an old list can't be replayed to bring a skill back), and the
// roles already built are rebuilt without the skills on it. Rubi sends the list when a version is withdrawn and
// again when it presents the library key, so a computer that was off catches up the next time Rubi reaches it.
func (h *Helper) setLibraryRevoked(args json.RawMessage) (any, error) {
	var in struct {
		IssuedAt int64    `json:"issued_at"`
		Revoked  []string `json:"revoked"`
		Sig      string   `json:"sig"`
	}
	if json.Unmarshal(args, &in) != nil || in.IssuedAt <= 0 || len(in.Revoked) > maxRevoked {
		return nil, errors.New("bad arguments")
	}
	for _, r := range in.Revoked {
		if !revokedRE.MatchString(r) {
			return nil, errors.New("bad arguments")
		}
	}
	if err := libsig.VerifyRevocations(h.libraryKey(), in.IssuedAt, in.Revoked, in.Sig); err != nil {
		return nil, err
	}
	var stale bool
	list := append([]string{}, in.Revoked...)
	sort.Strings(list)
	if err := h.Update(func(c *Config) {
		if in.IssuedAt < c.LibraryRevokedAt {
			stale = true
			return
		}
		c.LibraryRevoked, c.LibraryRevokedAt = list, in.IssuedAt
	}); err != nil {
		return nil, err
	}
	if stale {
		return nil, errors.New("a newer revocation list is already in force")
	}
	n := 0
	if h.Agent != nil {
		n = h.Agent.EnforceLibraryRevocations()
	}
	return map[string]any{"applied": true, "revoked": len(list), "skills_taken_out": n}, nil
}

func (h *Helper) hello() map[string]any {
	cfg := h.Config()
	return map[string]any{"name": cfg.Name, "version": version.Version, "shortcuts_folder": cfg.ShortcutsFolder,
		"can":  []string{"hue", "shortcuts", "claude", "claude-sessions", "claude-start", "claude-agent", "agent-setup", "t3-agent", "t3-setup"},
		"hook": h.hookStatus(), "hue_bridges": h.Hue.Bridges()}
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
	expired := errors.New("this pairing code has expired, was already used or was tried too often; open Rubi Home on that computer for a new one")
	if p := h.Fresh().Pending; p == nil || time.Now().After(p.Expires) {
		return nil, expired // nothing to change: no write to disk for a stranger's guess
	}
	var err error
	uerr := h.Update(func(c *Config) {
		p := c.Pending
		if p == nil || time.Now().After(p.Expires) {
			err = expired
			return
		}
		full := subtle.ConstantTimeCompare([]byte(p.Secret), []byte(in.Secret))
		short := 0
		if p.Pin != "" {
			short = subtle.ConstantTimeCompare([]byte(p.Pin), []byte(homeproto.NormalizePIN(in.Secret)))
		}
		if full|short != 1 {
			// A wrong secret: after maxPairAttempts the code is void and Rubi Home has to be opened again.
			if p.Attempts++; p.Attempts >= maxPairAttempts {
				c.Pending = nil
			}
			err = expired
			return
		}
		c.Pending = nil
		// A new pairing replaces every earlier one. Codes are made on this computer by its owner, so a new
		// one means "this Rubi, from now on": a pairing someone got from an earlier code ends here.
		c.LibraryKey = "" // the library key is pinned again by the new Rubi
		c.LibraryRevoked, c.LibraryRevokedAt = nil, 0
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

func (h *Helper) unsetHuePin(bridge string) error {
	return h.Update(func(c *Config) { delete(c.HuePins, bridge) })
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
