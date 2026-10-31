package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
)

// lockedAfter: Rubi counts as present while a claude-agent.next is open and this long after one ends.
var lockedAfter = 15 * time.Second

const (
	// DefaultWait is how long a perm.ask waits for a decision; the hook's own timeout is 300 s.
	DefaultWait  = 280 * time.Second
	maxPoll      = 25 * time.Second
	maxAsks      = 16
	maxOutbox    = 256
	maxItemBytes = 64 << 10
	maxInput     = 1 << 20
	maxPerPoll   = 32
)

// Messages of the denials Home makes itself (never a decision about the tool call).
const (
	DenyLocked   = "Rubi: Rubi isn't connected to this computer (it may be locked or offline), so the call was denied."
	DenyStopped  = "Rubi: Rubi stopped answering (it may have been locked or gone offline), so the call was denied."
	DenyNoAnswer = "Rubi: no answer in time, so the call was denied."
	DenyBusy     = "Rubi: too many calls are already waiting for approval; denied."
	DenyGone     = "Rubi: the request was cancelled."
	DenySelfTest = "Rubi: this is the setup self-test; every call is denied."
)

// Options configures a Manager.
type Options struct {
	// Dir is the helper's directory (the queue file lives there) and Protected more directories no
	// project may contain or be (the user's home, by default).
	Dir       string
	Protected []string
	// AuthDir is the shared sign-in's directory (Dir/agent-auth by default): no project may be, contain or be
	// inside it. The auth gateway reads the token in it; a VM never sees it.
	AuthDir string

	// Projects returns the directories agents may work in (config agent_projects); GitHosts the git hosts
	// the proxy and the credential helper serve (config agent_git_hosts).
	Projects func() []string
	GitHosts func() []string
	// NewVM makes the VM of a project.
	NewVM func(Project) VM
	// Cred is the credential source (nil: git credentials are refused).
	Cred CredSource
	// AllowedHosts are extra hosts the proxy allows for every project (config agent_allowed_hosts); the
	// well-known forges are added unless NoDefaultForges (agent_no_default_forges). Hosts are never read
	// from a project's files. AllowBypass (agent_allow_bypass, true when nil) says whether sessions may run
	// in bypassPermissions or dontAsk.
	AllowedHosts    func() []string
	NoDefaultForges func() bool
	AllowBypass     func() bool
	// Usage is the usage modes and raw limits (agent_default_mode, agent_bot_modes,
	// agent_max_sessions_per_project, agent_max_sessions, agent_max_subagents_per_session); nil means
	// medium for everyone. SetUsage saves a change of mode (the user's choice from the plugin's panel) and
	// SeeBot records a Bot met for the first time with the default mode; nil means no change is possible.
	Usage    func() UsageConfig
	SetUsage func(UsageChange) error
	// SetLimits saves a change of the session limits (claude-agent.limits, the user's choice from the panel).
	SetLimits func(LimitsChange) error
	SeeBot    func(id string)
	// RoleVerifier checks the signatures of library skills in roles (nil: every library skill is refused until
	// the library exists) and RoleMaxTotal is role_max_total (0: 50). The roles live in Dir/agent-roles.
	RoleVerifier roles.Verifier
	RoleMaxTotal int
	// RoleVMIdle is how long a role's VM may run with no slot and no queued item before it is stopped
	// (agent_role_vm_idle_minutes; nil or not positive: DefaultRoleVMIdle). The legacy role's VM is never stopped for it.
	RoleVMIdle func() time.Duration
	// Wait is how long an ask waits (DefaultWait when zero).
	Wait time.Duration
	// ProxyListen and ProxyVMHost: where the proxy listens and the host name the VM uses to reach it. An
	// empty ProxyListen means the Mac's address on the VM's network (the gateway the VM reports), or
	// 127.0.0.1:0 when the VM reports none; an empty ProxyVMHost means the listen host.
	ProxyListen string
	ProxyVMHost string
	// Resolve and Dial are the proxy's name lookup and upstream dialer (seams for tests).
	Resolve func(ctx context.Context, host string) ([]net.IP, error)
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	Now     func() time.Time
}

// Item is one thing for the plugin in claude-agent.next.
type Item struct {
	Type      string          `json:"type"` // perm.ask, cred.ask, net.ask, event, queue
	AskID     string          `json:"ask_id,omitempty"`
	Project   string          `json:"project,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	AgentID   string          `json:"agent_id,omitempty"`
	AgentType string          `json:"agent_type,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Role      string          `json:"role,omitempty"` // the role (id; the legacy role's is its project id)
	Via       string          `json:"via,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Cwd       string          `json:"cwd,omitempty"`
	Mode      string          `json:"mode,omitempty"`
	Host      string          `json:"host,omitempty"`
	Path      string          `json:"path,omitempty"`
	Op        string          `json:"op,omitempty"`
	ExpiresAt *time.Time      `json:"expires_at,omitempty"`
	Event     json.RawMessage `json:"event,omitempty"`
	Action    string          `json:"action,omitempty"` // queue items: queued, delivered, failed, expired
	Reason    string          `json:"reason,omitempty"` // queued: what it waits for, in plain words
	QueueID   string          `json:"queue_id,omitempty"`
	Error     string          `json:"error,omitempty"`
	At        time.Time       `json:"at"`
}

// slotState is one running top-level session (a slot in use). Subagents and forks inside it don't count.
type slotState struct {
	bot     string // the Bot that started the session ("" = the user, from the panel)
	project string
	role    string          // the role's key: its id, or the project id for the legacy role
	session string          // "" while a start hasn't got its session id yet
	turn    bool            // a turn is running
	last    time.Time       // the last sign of life (event, ask, delivery)
	bg      map[string]bool // background subagents still running
	sub     int             // subagents running now, as the runner counts them
}

type pendingAsk struct {
	item      Item
	project   string
	role      string // key
	delivered bool
	answer    chan AskResult
}

type projState struct {
	Project
	startMu sync.Mutex // one start at a time
	vm      VM
	runner  *Runner // guarded by Manager.mu
	proxy   *Proxy
	gateway *Gateway // the auth gateway of this start (gateway.go); guarded by Manager.mu like runner
	hosts   []string
	// lastUse is the last time something used a role's VM (guarded by Manager.mu); pins hold the role versions
	// this VM may be reading, released when it stops.
	lastUse time.Time
	pins    []func()
	// proxyEnv is the proxy variables the runner was started with (the proxy password is in them: never
	// logged); guarded by Manager.mu like runner.
	proxyEnv map[string]string
}

// key identifies the VM: the role's id, or the project id for the legacy role.
func (p *projState) key() string {
	if p.Role != "" {
		return p.Role
	}
	return p.ID
}

// netKey is where this VM's approved hosts are kept.
func (p *projState) netKey() string {
	if p.Role != "" {
		return roleNetKey(p.Role)
	}
	return p.Dir
}

// extraHosts are the hosts besides the base and git hosts: the owner's list, the forges, and what the
// owner approved for this project (or this role: an isolated role has its own approvals and its own hosts,
// added to its list only).
func (m *Manager) extraHosts(p *projState, roleHosts []string) []string {
	var out []string
	if m.opt.AllowedHosts != nil {
		out = append(out, m.opt.AllowedHosts()...)
	}
	if m.opt.NoDefaultForges == nil || !m.opt.NoDefaultForges() {
		out = append(out, ForgeHosts...)
	}
	out = append(out, roleHosts...)
	return append(out, m.net.hosts(p.netKey())...)
}

// Manager is the Mac side of the agent: projects, the slots, the queue, asks.
type Manager struct {
	opt Options
	q   *diskQueue
	net *netState
	// roles is the role store (Dir/agent-roles); nil with rolesErr when it couldn't be opened.
	roles    *roles.Store
	rolesErr error
	nameMu   sync.Mutex
	names    map[string]string // role id -> name, for messages (dropped when the role is put)

	mu    sync.Mutex
	projs map[string]*projState
	slots []*slotState // the sessions holding a slot, oldest first
	// sessionBot remembers which Bot started or first wrote to a session ("project|session"), for a session that
	// runs without a slot (bounded).
	sessionBot map[string]string
	asks       map[string]*pendingAsk
	out        []Item
	signal     chan struct{}
	polls      int
	lastPoll   time.Time
	login      json.RawMessage
	usage      usageDay
	lastCost   map[string]float64
	closed     bool
	self       *selfTest // the self-test that holds a slot, if any (setup.go)

	cancels    map[string]context.CancelFunc
	netFlights map[string]*netFlight
	netDenied  map[string]time.Time

	kick     chan struct{}
	stop     chan struct{}
	loopOnce sync.Once
	wg       sync.WaitGroup
}

type usageDay struct {
	Day          string  `json:"day"`
	Turns        int     `json:"turns"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// New makes a Manager and loads the queue file. Nothing runs until the first operation, or until Resume.
func New(opt Options) *Manager {
	if opt.Wait <= 0 {
		opt.Wait = DefaultWait
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Projects == nil {
		opt.Projects = func() []string { return nil }
	}
	if opt.GitHosts == nil {
		opt.GitHosts = func() []string { return nil }
	}
	m := &Manager{opt: opt, sessionBot: map[string]string{}, projs: map[string]*projState{}, asks: map[string]*pendingAsk{}, lastCost: map[string]float64{},
		kick: make(chan struct{}, 1), stop: make(chan struct{})}
	m.q = &diskQueue{}
	if opt.Dir != "" {
		m.q.path = filepath.Join(opt.Dir, QueueFile)
	}
	m.q.load()
	m.net = newNetState(opt.Dir)
	if opt.Dir != "" {
		m.roles, m.rolesErr = roles.Open(roles.Options{
			Dir: filepath.Join(opt.Dir, RolesDir),
			Projects: func() []string {
				var ids []string
				for _, p := range m.projects() {
					ids = append(ids, p.ID)
				}
				return ids
			},
			BaseHosts: func() []string {
				if opt.AllowedHosts == nil {
					return nil
				}
				return opt.AllowedHosts()
			},
			Verifier: opt.RoleVerifier,
			MaxTotal: opt.RoleMaxTotal,
			Now:      opt.Now,
			Logf:     log.Printf,
		})
	}
	return m
}

// Resume starts delivering a queue left by an earlier run (a no-op when it is empty).
func (m *Manager) Resume() {
	if m.q.len() > 0 {
		m.ensureLoop()
		m.wake()
	}
}

// Close stops the manager's work and every VM; the queue stays on disk.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	close(m.stop)
	var ps []*projState
	for _, p := range m.projs {
		ps = append(ps, p)
	}
	m.mu.Unlock()
	for _, p := range ps {
		m.shutdown(p)
	}
	m.wg.Wait()
}

func (m *Manager) shutdown(p *projState) {
	m.mu.Lock()
	r := p.runner
	p.runner = nil
	px, gw := p.proxy, p.gateway
	p.proxy, p.gateway = nil, nil
	m.unpinLocked(p)
	m.mu.Unlock()
	if r != nil {
		_ = r.Close()
	}
	if px != nil {
		px.Close()
	}
	if gw != nil {
		gw.Close()
	}
	if p.vm != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = p.vm.Stop(ctx)
		cancel()
	}
}

func (m *Manager) wake() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

func (m *Manager) ensureLoop() {
	m.loopOnce.Do(func() {
		m.wg.Add(1)
		go m.loop()
	})
}

// ---- projects ----

var idRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

// projects resolves the configured directories to projects. A directory that isn't an absolute existing
// directory, or that is, or contains, a protected one (the helper's own directory holds its keys) is
// skipped. The id is the directory's name, made unique with a number.
func (m *Manager) projects() []Project {
	var out []Project
	seen := map[string]bool{}
	dirs := map[string]bool{}
	for _, d := range m.opt.Projects() {
		if !filepath.IsAbs(d) {
			continue
		}
		real, err := filepath.EvalSymlinks(filepath.Clean(d))
		if err != nil {
			continue
		}
		if st, err := os.Stat(real); err != nil || !st.IsDir() || real == string(filepath.Separator) || dirs[real] || m.protected(real) {
			continue
		}
		dirs[real] = true
		id := strings.Trim(slugRE.ReplaceAllString(strings.ToLower(filepath.Base(real)), "-"), "-")
		if id == "" {
			id = "project"
		}
		base := id
		for n := 2; seen[id]; n++ {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		seen[id] = true
		out = append(out, Project{ID: id, Dir: real})
	}
	return out
}

// deniedTrees are directories under the user's home that never become a project, nor anything inside them.
var deniedTrees = []string{".ssh", "Library", ".gnupg", ".aws", ".config", ".kube"}

func badMountPath(dir string) bool {
	return strings.ContainsAny(dir, ",\x00\r\n") || strings.ContainsFunc(dir, unicode.IsControl)
}

func (m *Manager) protected(dir string) bool {
	if ProtectedDir(dir, append([]string{m.opt.Dir}, m.opt.Protected...)...) {
		return true
	}
	// Nothing inside the helper's directory (its keys, the shared sign-in) is a project either.
	for _, p := range []string{m.opt.Dir, m.opt.AuthDir, AuthDirOf(m.opt.Dir)} {
		if p == "" {
			continue
		}
		if rp, err := filepath.EvalSymlinks(p); err == nil {
			p = rp
		}
		if dir == p || strings.HasPrefix(dir, p+string(filepath.Separator)) || strings.HasPrefix(p, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// ProtectedDir says whether dir can never be a project: it can't be mounted, it is, or contains, the user's
// home (or a secrets directory in it), or it is, or contains, one of extra (the helper's own directory).
func ProtectedDir(dir string, extra ...string) bool {
	if badMountPath(dir) {
		return true
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rh, err := filepath.EvalSymlinks(home); err == nil {
			home = rh
		}
		if dir == home || strings.HasPrefix(home, dir+string(filepath.Separator)) {
			return true // the home itself, and /Users, /home, /
		}
		for _, t := range deniedTrees {
			tt := filepath.Join(home, t)
			if dir == tt || strings.HasPrefix(dir, tt+string(filepath.Separator)) || strings.HasPrefix(tt, dir+string(filepath.Separator)) {
				return true
			}
		}
	}
	for _, p := range extra {
		if p == "" {
			continue
		}
		if rp, err := filepath.EvalSymlinks(p); err == nil {
			p = rp
		}
		if dir == p || strings.HasPrefix(p, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// project is the project's legacy-role VM.
func (m *Manager) project(id string) (*projState, error) { return m.projectFor(id, "") }

// roleSel is what an operation's "role" argument names. id is "" for the project's legacy role.
type roleSel struct {
	id       string
	name     string
	archived bool
}

// selectRole resolves the role argument of an operation: missing (or the project's id, the legacy role's) is the
// legacy role; otherwise the role must exist and belong to the project.
func (m *Manager) selectRole(project, role string) (roleSel, error) {
	if role == "" || role == project {
		return roleSel{name: "default"}, nil
	}
	if !roles.IsID(role) {
		return roleSel{}, errors.New("bad role id")
	}
	if m.roles == nil {
		return roleSel{}, errors.New("roles aren't available here")
	}
	r, err := m.roles.Get(role, 0)
	if err != nil {
		return roleSel{}, fmt.Errorf("unknown role %q", clip(role, 64))
	}
	if r.Project != project {
		return roleSel{}, fmt.Errorf("role %s doesn't belong to project %q", clip(r.Name, 40), clip(project, 64))
	}
	m.nameMu.Lock()
	if m.names == nil {
		m.names = map[string]string{}
	}
	m.names[role] = r.Name
	m.nameMu.Unlock()
	return roleSel{id: role, name: r.Name, archived: r.Status == "archived"}, nil
}

// roleName is a role's name for messages and rows ("default" for the legacy role; the id when it can't be read).
func (m *Manager) roleName(project, id string) string {
	if id == "" || id == project {
		return "default"
	}
	m.nameMu.Lock()
	n, ok := m.names[id]
	m.nameMu.Unlock()
	if ok {
		return n
	}
	if m.roles != nil {
		if r, err := m.roles.Get(id, 0); err == nil {
			m.nameMu.Lock()
			if m.names == nil {
				m.names = map[string]string{}
			}
			m.names[id] = r.Name
			m.nameMu.Unlock()
			return r.Name
		}
	}
	return id
}

// projectFor is the VM state of one role of a project (role "" or the project's id: the legacy role), made on
// first use. Role names are not trusted by the VM layer beyond the container's name.
func (m *Manager) projectFor(id, role string) (*projState, error) {
	sel, err := m.selectRole(id, role)
	if err != nil {
		return nil, err
	}
	for _, p := range m.projects() {
		if p.ID != id {
			continue
		}
		key := id
		if sel.id != "" {
			if m.opt.Dir == "" {
				return nil, errors.New("roles aren't available here")
			}
			key = sel.id
			base := filepath.Join(m.opt.Dir, RolesDir, sel.id)
			p.Role, p.RoleName = sel.id, sel.name
			p.RoleDir, p.ClaudeDir = filepath.Join(base, "content"), filepath.Join(base, "claude")
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if ps := m.projs[key]; ps != nil && ps.Dir == p.Dir {
			return ps, nil
		}
		if m.opt.NewVM == nil {
			return nil, errors.New("no VM is configured")
		}
		ps := &projState{Project: p, vm: m.opt.NewVM(p)}
		m.projs[key] = ps
		return ps, nil
	}
	return nil, fmt.Errorf("unknown project %q", clip(id, 64))
}

// unpinLocked releases the role versions this VM held.
func (m *Manager) unpinLocked(p *projState) {
	for _, release := range p.pins {
		release()
	}
	p.pins = nil
}

// pinRole reads the role and marks its current version as in use by the VM about to start (the runner reads
// `current` at each query start, so every version put while the VM runs is pinned too, until it stops). The caller
// releases the pin if the start fails, else hands it to the VM (m.mu must not be held).
func (m *Manager) pinRole(p *projState) (*roles.Role, func(), error) {
	if m.roles == nil {
		return nil, nil, errors.New("roles aren't available here")
	}
	r, err := m.roles.Get(p.Role, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("role %q: %w", clip(p.Role, 64), err)
	}
	if r.Project != p.ID {
		return nil, nil, fmt.Errorf("role %s doesn't belong to project %q", clip(r.Name, 40), clip(p.ID, 64))
	}
	return r, m.roles.Pin(p.Role, r.Version), nil
}

// rolePut is called after a role.put went through: the name may have changed and a running VM may read the new version.
func (m *Manager) rolePut(res any) {
	pr, ok := res.(*roles.PutResult)
	if !ok || pr == nil {
		return
	}
	m.nameMu.Lock()
	delete(m.names, pr.ID)
	m.nameMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.projs[pr.ID]; p != nil && p.runner != nil {
		p.pins = append(p.pins, m.roles.Pin(pr.ID, pr.Version))
	}
}

// ensure starts the project's VM and runner if they aren't running.
func (m *Manager) ensure(ctx context.Context, p *projState) (*Runner, error) {
	p.startMu.Lock()
	defer p.startMu.Unlock()
	m.mu.Lock()
	r := p.runner
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return nil, errors.New("Rubi Home is stopping")
	}
	if r != nil {
		select {
		case <-r.Done():
		default:
			m.touch(p)
			return r, nil
		}
	}
	var roleHosts []string
	var unpin func()
	if p.Role == "" {
		if err := prepareWorkDir(p.Dir); err != nil {
			return nil, err
		}
	} else {
		// An isolated role writes nothing under /work/.rubi: its config directory is outside the project.
		for _, d := range []string{p.RoleDir, p.ClaudeDir} {
			if st, err := os.Stat(d); err != nil || !st.IsDir() {
				return nil, fmt.Errorf("role %s has no directory %s to mount", clip(p.RoleName, 40), filepath.Base(d))
			}
		}
		role, release, err := m.pinRole(p)
		if err != nil {
			return nil, err
		}
		roleHosts, unpin = role.Hosts, release
		defer func() {
			if unpin != nil {
				unpin() // the start failed
			}
		}()
	}
	// A VM start can take minutes (the image); it must not die with the request that asked for it.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	defer cancel()
	token, err := NewToken()
	if err != nil {
		return nil, err
	}
	proxyToken, err := NewToken()
	if err != nil {
		return nil, err
	}
	hosts := AllowList(m.opt.GitHosts(), m.extraHosts(p, roleHosts))
	// The proxy starts once the VM is up: the Mac's address on a host-only network (its gateway) exists only
	// while a VM is attached to it.
	var px *Proxy
	var penv map[string]string
	var gw *Gateway
	gwSecret, err := NewToken()
	if err != nil {
		return nil, err
	}
	authDir := m.opt.AuthDir
	if authDir == "" {
		authDir = AuthDirOf(m.opt.Dir)
	}
	prepare := func(ctx context.Context, gateway string) (map[string]string, error) {
		listen := m.opt.ProxyListen
		if listen == "" && gateway != "" {
			listen = net.JoinHostPort(gateway, "0")
		}
		var err error
		px, err = StartProxy(ProxyConfig{Listen: listen, AuthToken: proxyToken, Hosts: hosts, Exempt: m.opt.GitHosts(),
			Resolve: m.opt.Resolve, Dial: m.opt.Dial, Logf: log.Printf,
			Ask: func(ctx context.Context, host string) bool { return m.askHost(ctx, p, host) }})
		if err != nil {
			return nil, fmt.Errorf("proxy: %w", err)
		}
		_, port, _ := net.SplitHostPort(px.Addr())
		host := m.opt.ProxyVMHost
		if host == "" {
			host, _, _ = net.SplitHostPort(px.Addr())
		}
		proxyURL := "http://rubi:" + proxyToken + "@" + net.JoinHostPort(host, port)
		// The auth gateway: the VM talks to the model API only through it, with a secret that is good for this start.
		// It listens on the proxy's host; claude is told not to send it through the CONNECT proxy (NO_PROXY).
		// The gateway has its own listener, always on the Mac's address on the VM network when the VM reports one
		// (the CONNECT proxy may have been given another address by agent_proxy_listen); the VM reaches it there.
		lhost, _, _ := net.SplitHostPort(px.Addr())
		vmHost := host
		if gateway != "" {
			lhost, vmHost = gateway, gateway
		}
		upstream, client := gatewaySeam()
		gw, err = StartGateway(GatewayConfig{Listen: net.JoinHostPort(lhost, "0"), Secret: gwSecret, TokenFile: TokenFileOf(authDir),
			upstream: upstream, client: client, Logf: log.Printf})
		if err != nil {
			px.Close()
			return nil, fmt.Errorf("gateway: %w", err)
		}
		_, gport, _ := net.SplitHostPort(gw.Addr())
		penv = map[string]string{"HTTPS_PROXY": proxyURL, "https_proxy": proxyURL,
			"ANTHROPIC_BASE_URL": "http://" + net.JoinHostPort(vmHost, gport), "CLAUDE_CODE_OAUTH_TOKEN": gwSecret,
			"NO_PROXY": vmHost, "no_proxy": vmHost}
		return penv, nil
	}
	env := map[string]string{"RUBI_RUN_TOKEN": token, "GIT_TERMINAL_PROMPT": "0"}
	if p.Role != "" {
		env["RUBI_ROLE_DIR"], env["RUBI_CLAUDE_DIR"] = RoleMountDir, RoleClaudeDir
	}
	conn, err := p.vm.Start(sctx, StartSpec{Project: p.Project, Env: env, Prepare: prepare})
	if err != nil {
		if px != nil {
			px.Close()
		}
		if gw != nil {
			gw.Close()
		}
		return nil, fmt.Errorf("starting the VM: %w", err)
	}
	r, err = Connect(sctx, conn, token, &handler{m: m, p: p})
	if err != nil {
		px.Close()
		gw.Close()
		_ = p.vm.Stop(sctx)
		return nil, err
	}
	m.mu.Lock()
	p.runner, p.proxy, p.gateway, p.hosts, p.proxyEnv = r, px, gw, hosts, penv
	p.lastUse = m.opt.Now()
	if unpin != nil {
		p.pins = append(p.pins, unpin)
		unpin = nil
	}
	m.mu.Unlock()
	if p.Role != "" {
		m.ensureLoop() // the loop stops an idle role VM
	}
	go func() {
		<-r.Done()
		m.runnerGone(p, r)
	}()
	return r, nil
}

func (m *Manager) runnerGone(p *projState, r *Runner) {
	m.mu.Lock()
	if p.runner != r {
		m.mu.Unlock()
		return
	}
	p.runner = nil
	px, gw := p.proxy, p.gateway
	p.proxy, p.gateway = nil, nil
	m.unpinLocked(p)
	if m.dropRoleSlotsLocked(p.key()) {
		m.pushLocked(Item{Type: "event", Project: p.ID, Role: p.key(), Event: json.RawMessage(`{"kind":"status","status":"closed","reason":"runner ended"}`)})
	}
	m.mu.Unlock()
	if px != nil {
		px.Close()
	}
	if gw != nil {
		gw.Close() // the per-start secret dies with the runner
	}
	m.wake()
}

// ---- items, asks, poll presence ----

// pushLocked adds an item for the plugin; when full the oldest event goes (asks are bounded elsewhere).
func (m *Manager) pushLocked(it Item) {
	it.At = m.opt.Now().UTC()
	if len(m.out) >= maxOutbox {
		for i, o := range m.out {
			if o.Type == "event" || o.Type == "queue" {
				m.out = append(m.out[:i], m.out[i+1:]...)
				break
			}
		}
		if len(m.out) >= maxOutbox {
			m.out = m.out[1:]
		}
	}
	m.out = append(m.out, it)
	if m.signal != nil {
		close(m.signal)
		m.signal = nil
	}
}

func (m *Manager) connectedLocked() bool {
	return m.polls > 0 || (!m.lastPoll.IsZero() && m.opt.Now().Sub(m.lastPoll) <= lockedAfter)
}

func (m *Manager) connected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connectedLocked()
}

func deny(msg string) AskResult { return AskResult{Behavior: "deny", Message: msg} }

// waitDecision queues the ask item and waits; anything but a decision from the plugin is a denial.
func (m *Manager) waitDecision(ctx context.Context, project string, it Item) AskResult {
	m.mu.Lock()
	if st := m.self; st != nil && st.project == project { // the whole project, whatever the role
		// A self-test (setup.go) denies everything itself: the question never reaches the plugin, so no
		// decision of the plugin's can answer it.
		if it.Type == "perm.ask" {
			st.denied = append(st.denied, it.Tool)
		}
		m.mu.Unlock()
		return deny(DenySelfTest)
	}
	if !m.connectedLocked() {
		m.mu.Unlock()
		return deny(DenyLocked)
	}
	if len(m.asks) >= maxAsks {
		m.mu.Unlock()
		return deny(DenyBusy)
	}
	id := "aa_" + rand.Text()
	it.AskID = id
	exp := m.opt.Now().Add(m.opt.Wait).UTC()
	it.ExpiresAt = &exp
	rk := it.Role
	if rk == "" {
		rk = project
	}
	pa := &pendingAsk{item: it, project: project, role: rk, answer: make(chan AskResult, 1)}
	m.asks[id] = pa
	m.pushLocked(it)
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.asks, id)
		for i, o := range m.out {
			if o.AskID == id {
				m.out = append(m.out[:i], m.out[i+1:]...)
				break
			}
		}
		m.mu.Unlock()
	}()
	deadline := time.NewTimer(m.opt.Wait)
	defer deadline.Stop()
	alive := time.NewTicker(time.Second)
	defer alive.Stop()
	for {
		select {
		case r := <-pa.answer:
			return r
		case <-ctx.Done():
			return deny(DenyGone)
		case <-deadline.C:
			return deny(DenyNoAnswer)
		case <-alive.C:
			if !m.connected() {
				return deny(DenyStopped)
			}
		}
	}
}

func cleanMessage(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 500 {
		s = string(r[:500])
	}
	return s
}

// Decide answers a perm.ask or cred.ask the plugin was handed. It works once.
func (m *Manager) Decide(askID, behavior, message string, updated json.RawMessage) error {
	if behavior != "allow" && behavior != "deny" {
		return errors.New(`behavior must be "allow" or "deny"`)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	pa := m.asks[askID]
	if pa == nil || !pa.delivered {
		return errors.New("unknown or expired request")
	}
	delete(m.asks, askID)
	if pa.item.ExpiresAt != nil && m.opt.Now().After(*pa.item.ExpiresAt) {
		pa.answer <- deny(DenyNoAnswer)
		return errors.New("the request has expired")
	}
	if behavior == "deny" {
		msg := cleanMessage(message)
		if msg == "" {
			msg = "denied."
		}
		pa.answer <- deny("Rubi: " + msg)
		return nil
	}
	res := AskResult{Behavior: "allow"}
	if len(updated) > 0 && string(updated) != "null" {
		if len(updated) > maxInput || updated[0] != '{' || !json.Valid(updated) {
			pa.answer <- deny("Rubi: the approved input was malformed, so the call was denied.")
			return errors.New("updated_input must be a JSON object")
		}
		res.UpdatedInput = updated
	}
	pa.answer <- res
	return nil
}

// Next hands the plugin what is waiting, waiting up to wait (at most 25 s) for the first item. While it
// is called, Rubi counts as present.
func (m *Manager) Next(ctx context.Context, wait time.Duration) ([]Item, error) {
	if wait <= 0 || wait > maxPoll {
		wait = maxPoll
	}
	m.mu.Lock()
	m.polls++
	m.lastPoll = m.opt.Now()
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.polls--
		m.lastPoll = m.opt.Now()
		m.mu.Unlock()
	}()
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		m.mu.Lock()
		if len(m.out) > 0 {
			n := min(len(m.out), maxPerPoll)
			items := append([]Item(nil), m.out[:n]...)
			m.out = m.out[n:]
			for _, it := range items {
				if pa := m.asks[it.AskID]; pa != nil && it.AskID != "" {
					pa.delivered = true
				}
			}
			m.mu.Unlock()
			return items, nil
		}
		if m.signal == nil {
			m.signal = make(chan struct{})
		}
		sig := m.signal
		m.mu.Unlock()
		select {
		case <-sig:
		case <-t.C:
			return []Item{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// ---- the runner's side: handler ----

type handler struct {
	m *Manager
	p *projState
}

func (h *handler) Cancel(askID string) {
	h.m.mu.Lock()
	cancel := h.m.cancels[h.p.key()+"|"+askID]
	h.m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (h *handler) Ask(ctx context.Context, a AskParams) AskResult {
	// The runner gives a call up with perm.cancel {ask_id}: that ends the wait and drops the item.
	ctx, cancel := context.WithCancel(ctx)
	key := h.p.key() + "|" + a.AskID
	h.m.mu.Lock()
	if h.m.cancels == nil {
		h.m.cancels = map[string]context.CancelFunc{}
	}
	h.m.cancels[key] = cancel
	h.m.mu.Unlock()
	defer func() {
		h.m.mu.Lock()
		delete(h.m.cancels, key)
		h.m.mu.Unlock()
		cancel()
	}()
	if len(a.Input) > maxInput {
		return deny("Rubi: the tool input is too large to review, so the call was denied.")
	}
	it := Item{Type: "perm.ask", Project: h.p.ID, Role: h.p.key(), SessionID: a.SessionID, AgentID: a.AgentID, AgentType: a.AgentType,
		Tool: a.Tool, ToolUseID: a.ToolUseID, Via: a.Via, Input: RedactCredentials(a.Input), Cwd: a.Cwd, Mode: a.Mode}
	return h.m.waitDecision(ctx, h.p.ID, it)
}

func (h *handler) Event(ev Event) { h.m.onEvent(h.p, ev) }

func (h *handler) Credential(ctx context.Context, c CredParams) (CredResult, error) {
	return h.m.credential(ctx, h.p, c)
}

func (m *Manager) credential(ctx context.Context, p *projState, c CredParams) (CredResult, error) {
	switch c.Op {
	case "store", "erase":
		return CredResult{}, nil
	case "get":
	default:
		return CredResult{}, errors.New("unknown credential operation")
	}
	kv, err := parseCredInput(c.Input)
	if err != nil {
		return CredResult{}, err
	}
	host := normalizeHost(kv["host"])
	if kv["protocol"] != "https" || host == "" {
		return CredResult{}, errors.New("only https git hosts are served")
	}
	listed := false
	for _, g := range m.opt.GitHosts() {
		listed = listed || normalizeHost(g) == host
	}
	if !listed {
		return CredResult{}, errors.New("this git host isn't configured (agent_git_hosts)")
	}
	if m.opt.Cred == nil {
		return CredResult{}, errors.New("no git credential source is configured")
	}
	if c.Purpose == "push" {
		it := Item{Type: "cred.ask", Project: p.ID, Role: p.key(), Host: host, Path: clip(strings.TrimSpace(kv["path"]), 200), Op: "push"}
		if r := m.waitDecision(ctx, p.ID, it); r.Behavior != "allow" {
			msg := r.Message
			if msg == "" {
				msg = "Rubi: the push wasn't approved."
			}
			return CredResult{}, errors.New(msg)
		}
		if cr, ok := m.opt.Cred.Push(host); ok {
			return CredResult{Output: credOutput(host, cr)}, nil
		}
		return CredResult{}, errors.New("no push credential for this host")
	}
	if cr, ok := m.opt.Cred.Read(host); ok {
		return CredResult{Output: credOutput(host, cr)}, nil
	}
	return CredResult{}, nil // none: git fails without prompting
}

// ---- events and the slots ----

// usageNow is the usage configuration, read at each use so a change applies to the next start.
func (m *Manager) usageNow() UsageConfig {
	if m.opt.Usage != nil {
		return m.opt.Usage()
	}
	return UsageConfig{}
}

func sessions(n int) string {
	if n == 1 {
		return "1 session"
	}
	return fmt.Sprintf("%d sessions", n)
}

func botName(bot string) string {
	if bot == "" {
		return "your panel sessions"
	}
	return "bot " + bot
}

// fullLocked says why a new top-level session of the Bot in the project can't run now ("" when it can):
// the project has its sessions already, the Bot has (its mode's limit), or the computer has. A start also
// waits while another start of the project hasn't got its session id, so an event is never matched to the
// wrong one.
func (m *Manager) fullLocked(c UsageConfig, project, rk, bot string, start bool) string {
	l := c.For(bot)
	n, nr, nb, pending := 0, 0, 0, false
	for _, s := range m.slots {
		if s.project == project {
			n++
		}
		if s.role == rk {
			nr++
			pending = pending || s.session == ""
		}
		if s.bot == bot {
			nb++
		}
	}
	switch {
	case m.self != nil && m.self.project == project: // its questions are answered by the self-test, never by a user
		return "waiting: project " + project + " is running its self-test"
	case start && pending:
		return fmt.Sprintf("waiting: role %s (%s) is still starting a session", m.roleName(project, rk), project)
	case nr >= l.PerProfile:
		return fmt.Sprintf("waiting: role %s (%s) already has %s", m.roleName(project, rk), project, sessions(nr))
	case l.PerProject > 0 && n >= l.PerProject:
		return "waiting: project " + project + " already has " + sessions(n)
	case l.Sessions > 0 && nb >= l.Sessions:
		return fmt.Sprintf("waiting: %s already has %s, the limit of its %s mode", botName(bot), sessions(nb), l.Mode)
	case c.Machine() > 0 && len(m.slots) >= c.Machine():
		return "waiting: the computer's session limit is reached"
	}
	return ""
}

// exactLocked is the slot of a known session.
func (m *Manager) exactLocked(rk, session string) *slotState {
	for _, s := range m.slots {
		if s.role == rk && session != "" && s.session == session {
			return s
		}
	}
	return nil
}

// ownerLocked is the slot an event of the session belongs to: the session's own, else the project's slot
// that hasn't got its id yet (a start in flight), else, for an event without a session, the project's only one.
func (m *Manager) ownerLocked(p *projState, session string) *slotState {
	if s := m.exactLocked(p.key(), session); s != nil {
		return s
	}
	var found *slotState
	n := 0
	for _, s := range m.slots {
		if s.role != p.key() {
			continue
		}
		n++
		if s.session == "" || session == "" {
			found = s
		}
	}
	if session == "" && n != 1 {
		return nil
	}
	return found
}

// reasonLocked says why a queue item can't be delivered now ("" when it can). A send to a session that
// holds a slot already runs (owner is its slot).
func (m *Manager) reasonLocked(c UsageConfig, it QueueItem) (reason string, owner *slotState) {
	if it.Op == "send" {
		if s := m.exactLocked(it.rk(), it.Session); s != nil {
			return "", s
		}
	}
	return m.fullLocked(c, it.Project, it.rk(), it.Bot, it.Op == "start"), nil
}

// runnableLocked says whether some queued item could be delivered now (a drain is then due).
func (m *Manager) runnableLocked(c UsageConfig) bool {
	for _, q := range m.q.list() {
		if r, _ := m.reasonLocked(c, q); r == "" {
			return true
		}
	}
	return false
}

func (m *Manager) releaseLocked(s *slotState) {
	for i, o := range m.slots {
		if o == s {
			m.slots = append(m.slots[:i:i], m.slots[i+1:]...)
			break
		}
	}
	if p := m.projs[s.role]; p != nil {
		p.lastUse = m.opt.Now() // the idle clock of a role VM starts when its last session ends
	}
	m.wake()
}

// touch notes that something used the VM now.
func (m *Manager) touch(p *projState) {
	m.mu.Lock()
	p.lastUse = m.opt.Now()
	m.mu.Unlock()
}

// dropRoleSlotsLocked frees every slot of the role (its runner ended) and says whether there was one.
func (m *Manager) dropRoleSlotsLocked(rk string) bool {
	keep := m.slots[:0:0]
	for _, s := range m.slots {
		if s.role != rk {
			keep = append(keep, s)
		}
	}
	gone := len(keep) != len(m.slots)
	m.slots = keep
	return gone
}

func (m *Manager) onEvent(p *projState, ev Event) {
	var f struct {
		Status string `json:"status"`
		State  string `json:"state"`
		Tasks  *[]struct {
			ID string `json:"task_id"`
		} `json:"tasks"`
		Result    string  `json:"result"`
		IsError   bool    `json:"is_error"`
		Count     int     `json:"count"`
		TotalCost float64 `json:"total_cost_usd"`
		Usage     struct {
			In  int64 `json:"input_tokens"`
			Out int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(ev.Raw, &f)
	if f.State != "" {
		f.Status = f.State // the runner says "state"; "status" is accepted too
	}
	var uc UsageConfig
	if ev.Kind == "status" && f.Status == "busy" {
		uc = m.usageNow() // read before the lock: it may read the configuration file
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.self; st != nil && st.project == p.ID && p.Role == "" {
		st.event(ev, f.Result, f.IsError, f.Status) // the self-test's own events stay with it
		return
	}
	s := m.ownerLocked(p, ev.SessionID)
	if s != nil {
		s.last = m.opt.Now()
	}
	switch ev.Kind {
	case "status":
		switch f.Status {
		case "busy":
			if s == nil {
				// A session that runs without a slot (the runner resumed it, or Home restarted): it is booked, charged to
				// the Bot that started it when Home remembers which, else to the user's own. It is booked even over a limit,
				// and logged, so that it is visible and counted; the limits then hold back new work until it ends.
				bot := m.sessionBot[p.key()+"|"+ev.SessionID]
				if why := m.fullLocked(uc, p.ID, p.key(), bot, false); why != "" {
					log.Printf("agent: session %q of project %q is running over the limits (%s); counted against %s", clip(ev.SessionID, 64), clip(p.ID, 64), strings.TrimPrefix(why, "waiting: "), botName(bot))
				}
				m.slots = append(m.slots, &slotState{bot: bot, project: p.ID, role: p.key(), session: ev.SessionID, turn: true, last: m.opt.Now()})
			} else {
				s.turn = true
				if s.session == "" {
					s.session = ev.SessionID
				}
			}
		case "idle":
			if s != nil {
				s.turn = false
				m.maybeReleaseLocked(s)
			}
		case "closed":
			if s != nil {
				m.releaseLocked(s)
			}
		}
	case "background":
		if s != nil && f.Tasks != nil {
			// The runner lists every task still alive: that is the whole set.
			s.bg = map[string]bool{}
			for i, t := range *f.Tasks {
				if t.ID != "" && i < 1024 {
					s.bg[t.ID] = true
				}
			}
			m.maybeReleaseLocked(s)
		} else if s != nil && ev.AgentID != "" {
			if ev.Done {
				delete(s.bg, ev.AgentID)
				m.maybeReleaseLocked(s)
			} else {
				if s.bg == nil {
					s.bg = map[string]bool{}
				}
				if len(s.bg) < 1024 {
					s.bg[ev.AgentID] = true
				}
			}
		}
	case "subagents":
		if s != nil {
			s.sub = min(max(f.Count, 0), 1<<20)
		}
	case "result":
		if ev.AgentID != "" {
			if s != nil {
				delete(s.bg, ev.AgentID)
				m.maybeReleaseLocked(s)
			}
		} else {
			m.addUsageLocked(ev.SessionID, f.Usage.In, f.Usage.Out, f.TotalCost)
		}
	}
	raw := RedactCredentials(ev.Raw)
	if len(raw) > maxItemBytes {
		raw, _ = json.Marshal(map[string]any{"kind": ev.Kind, "session_id": ev.SessionID, "truncated": true})
	}
	m.pushLocked(Item{Type: "event", Project: p.ID, Role: p.key(), SessionID: ev.SessionID, AgentID: ev.AgentID, Event: raw})
}

// maybeReleaseLocked frees the slot when no turn runs and no background subagent is left.
func (m *Manager) maybeReleaseLocked(s *slotState) {
	if !s.turn && len(s.bg) == 0 {
		m.releaseLocked(s)
	}
}

func (m *Manager) addUsageLocked(session string, in, out int64, cost float64) {
	day := m.opt.Now().Format("2006-01-02")
	if m.usage.Day != day {
		m.usage = usageDay{Day: day}
		m.lastCost = map[string]float64{}
	}
	m.usage.Turns++
	m.usage.InputTokens += in
	m.usage.OutputTokens += out
	if d := cost - m.lastCost[session]; d > 0 { // total_cost_usd is cumulative per query
		m.usage.CostUSD += d
	}
	if len(m.lastCost) < 1024 {
		m.lastCost[session] = cost
	}
}

// ---- submit, deliver, drain ----

func (m *Manager) emitQueue(action string, it QueueItem, extra func(*Item)) {
	n := Item{Type: "queue", Action: action, QueueID: it.ID, Project: it.Project, Role: it.rk(), SessionID: it.Session, Op: it.Op}
	if extra != nil {
		extra(&n)
	}
	m.mu.Lock()
	m.pushLocked(n)
	m.mu.Unlock()
}

// submit delivers a start or send now when a slot allows, otherwise queues it on disk. The result is
// what the op returns.
func (m *Manager) submit(ctx context.Context, it QueueItem) (map[string]any, error) {
	p, err := m.projectFor(it.Project, it.Role)
	if err != nil {
		return nil, err
	}
	m.ensureLoop()
	it.ID, it.At = newQueueID(), m.opt.Now().UTC()
	uc := m.usageNow() // read before the lock: it may read the configuration file
	m.mu.Lock()
	// A new item never jumps ahead of a queued one that could run now.
	reason, owner := m.reasonLocked(uc, it)
	var slot *slotState
	prevTurn := false
	switch {
	case owner != nil:
		prevTurn, owner.turn = owner.turn, true
	case reason == "" && !m.runnableLocked(uc):
		slot = m.takeSlotLocked(it)
	case reason == "":
		reason = "waiting: earlier messages are queued first"
	}
	m.mu.Unlock()
	if owner != nil || slot != nil {
		res, err := m.deliver(ctx, p, it, slot)
		if err != nil {
			m.mu.Lock()
			if slot != nil {
				m.releaseLocked(slot)
			} else if owner != nil {
				owner.turn = prevTurn // the send never started a turn
				m.maybeReleaseLocked(owner)
			}
			m.mu.Unlock()
			return nil, err
		}
		if it.Op == "send" {
			res["queued"], res["position"], res["delivered"] = true, 0, true
		}
		m.wake() // a start that got its id may let a waiting start of the project go
		return res, nil
	}
	pos, err := m.q.add(it)
	if err != nil {
		return nil, err
	}
	m.emitQueue("queued", it, func(n *Item) { n.Reason = reason })
	return map[string]any{"queued": true, "position": pos, "delivered": false, "reason": reason}, nil
}

// takeSlotLocked books a slot for a start or a send (the caller checked there is room).
func (m *Manager) takeSlotLocked(it QueueItem) *slotState {
	s := &slotState{bot: it.Bot, project: it.Project, role: it.rk(), session: it.Session, turn: true, last: m.opt.Now()}
	m.slots = append(m.slots, s)
	return s
}

// deliver hands the item to the runner; slot is the slot a start booked (nil for a send to a running session).
func (m *Manager) deliver(ctx context.Context, p *projState, it QueueItem, slot *slotState) (map[string]any, error) {
	if it.Op == "start" && p.Role != "" {
		if sel, err := m.selectRole(p.ID, p.Role); err != nil || sel.archived {
			return nil, archivedErr(p.RoleName, err)
		}
	}
	r, err := m.ensure(ctx, p)
	if err != nil {
		return nil, err
	}
	switch it.Op {
	case "start":
		var res struct {
			ID string `json:"id"`
		}
		err := r.Call(ctx, "session.start", map[string]any{"prompt": it.Text, "name": it.Name, "from": it.From, "mode": it.Mode,
			"max_subagents": m.usageNow().For(it.Bot).Subagents}, &res)
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		if slot != nil && slot.session == "" {
			slot.session = res.ID
		}
		m.rememberBotLocked(p.key(), res.ID, it.Bot, true)
		m.mu.Unlock()
		return map[string]any{"id": res.ID}, nil
	default:
		var res json.RawMessage
		err := r.Call(ctx, "session.send", map[string]any{"id": it.Session, "text": it.Text, "from": it.From, "steer": it.Steer,
			"max_subagents": m.usageNow().For(it.Bot).Subagents}, &res)
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.rememberBotLocked(p.key(), it.Session, it.Bot, false)
		m.mu.Unlock()
		return map[string]any{}, nil
	}
}

// rememberBotLocked notes which Bot a session belongs to: the one that started it, or (for a session of unknown
// origin) the first Bot that wrote to it. A user's message to a Bot's session doesn't change it.
func (m *Manager) rememberBotLocked(project, session, bot string, started bool) {
	if session == "" || bot == "" && !started {
		return
	}
	key := project + "|" + session
	if _, known := m.sessionBot[key]; known && !started {
		return
	}
	if len(m.sessionBot) >= 4096 {
		for k := range m.sessionBot { // bounded: drop an arbitrary old entry
			delete(m.sessionBot, k)
			break
		}
	}
	m.sessionBot[key] = bot
}

func (m *Manager) loop() {
	defer m.wg.Done()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		m.drain()
		select {
		case <-m.kick:
		case <-tick.C:
			m.checkSlot()
			m.sweepIdle()
		case <-m.stop:
			return
		}
	}
}

// DefaultRoleVMIdle is how long a role's VM may run with no session and nothing queued (agent_role_vm_idle_minutes).
const DefaultRoleVMIdle = 15 * time.Minute

func (m *Manager) roleIdle() time.Duration {
	if m.opt.RoleVMIdle != nil {
		if d := m.opt.RoleVMIdle(); d > 0 {
			return d
		}
	}
	return DefaultRoleVMIdle
}

// roleBusyLocked says whether the role has a slot or a queued item.
func (m *Manager) roleBusyLocked(rk string) bool {
	for _, s := range m.slots {
		if s.role == rk {
			return true
		}
	}
	for _, it := range m.q.list() {
		if it.rk() == rk {
			return true
		}
	}
	return false
}

func (m *Manager) idleLocked(p *projState, idle time.Duration) bool {
	return p.Role != "" && p.runner != nil && m.opt.Now().Sub(p.lastUse) >= idle && !m.roleBusyLocked(p.key()) && !m.closed
}

// sweepIdle stops the VM of every role that has had no slot and no queued item for the idle time. The legacy
// role's VM stays up. A VM stopped here starts again on the next use.
func (m *Manager) sweepIdle() {
	idle := m.roleIdle() // read before the lock: it may read the configuration file
	m.mu.Lock()
	var cands []*projState
	for _, p := range m.projs {
		if m.idleLocked(p, idle) {
			cands = append(cands, p)
		}
	}
	m.mu.Unlock()
	for _, p := range cands {
		p.startMu.Lock() // no start in flight; a use after this waits for the VM to be gone, then starts it again
		m.mu.Lock()
		ok := m.idleLocked(p, idle)
		m.mu.Unlock()
		if ok {
			log.Printf("agent: the VM of role %q (project %q) was idle for %v; stopping it", clip(p.RoleName, 40), clip(p.ID, 64), idle)
			m.shutdown(p)
		}
		p.startMu.Unlock()
	}
}

// startTimeout is how long handing a start or send to the runner may take (it can boot the VM).
const startTimeout = 12 * time.Minute

// drain delivers queued items while a slot is free. The queue is first in, first out, except that an
// item waiting on a full project doesn't hold up the items behind it: the first item that can run now
// goes, so an item for a project with a free slot is never blocked by an earlier one waiting on another
// project. Items of one project and Bot keep their order (they wait on the same slots), and when only the
// total limit is reached nothing runs, so the oldest waiting item takes the next slot freed.
func (m *Manager) drain() {
	for {
		for _, it := range m.q.sweep(m.opt.Now()) {
			m.emitQueue("expired", it, nil)
		}
		uc := m.usageNow() // once per pass, read before the lock: it may read the configuration file
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return
		}
		var owner *slotState
		prevTurn := false
		it, ok := m.q.take(func(q QueueItem) bool {
			r, o := m.reasonLocked(uc, q)
			owner = o
			return r == ""
		})
		if !ok {
			m.mu.Unlock()
			return
		}
		var slot *slotState
		if owner != nil {
			prevTurn, owner.turn = owner.turn, true
		} else {
			slot = m.takeSlotLocked(it)
		}
		m.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
		var err error
		p, perr := m.projectFor(it.Project, it.Role)
		if perr != nil {
			err = perr
		} else {
			_, err = m.deliver(ctx, p, it, slot)
		}
		cancel()
		if err != nil {
			m.mu.Lock()
			if slot != nil {
				m.releaseLocked(slot)
			} else if owner != nil {
				owner.turn = prevTurn
				m.maybeReleaseLocked(owner)
			}
			m.mu.Unlock()
			m.emitQueue("failed", it, func(n *Item) { n.Error = clip(err.Error(), 300) })
			continue
		}
		m.emitQueue("delivered", it, nil)
	}
}

// ---- operations ----

var modes = map[string]bool{"default": true, "acceptEdits": true, "plan": true, "bypassPermissions": true, "dontAsk": true}

var botRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@-]{0,63}$`)

// okBot accepts a Bot id (a stable name the plugin gives) or "" (the user's own).
func okBot(s string) bool { return s == "" || botRE.MatchString(s) }

// seeBot records a Bot met for the first time (it gets the default mode, and the panel lists it).
func (m *Manager) seeBot(bot string) {
	if bot == "" || m.opt.SeeBot == nil {
		return
	}
	if _, ok := m.usageNow().Bots[bot]; !ok {
		m.opt.SeeBot(bot)
	}
}

func okID(s string) bool { return idRE.MatchString(s) }

// Handle serves one claude-agent.* operation.
func (m *Manager) Handle(ctx context.Context, op string, args json.RawMessage) (any, error) {
	if strings.HasPrefix(strings.TrimPrefix(op, "claude-agent."), "role.") {
		if m.roles == nil {
			if m.rolesErr != nil {
				return nil, fmt.Errorf("roles aren't available: %s", clip(m.rolesErr.Error(), 120))
			}
			return nil, errors.New("roles aren't available here")
		}
		res, err := m.roles.Handle(strings.TrimPrefix(op, "claude-agent."), args)
		if err == nil && strings.TrimPrefix(op, "claude-agent.") == "role.put" {
			m.rolePut(res)
		}
		return res, err
	}
	var in struct {
		Project     string          `json:"project"`
		Role        string          `json:"role"` // a role id; missing = the project's legacy role
		PerProfile  *int            `json:"per_profile"`
		PerProject  *int            `json:"per_project"`
		Total       *int            `json:"total"`
		ID          string          `json:"id"`
		Limit       int             `json:"limit"`
		Prompt      string          `json:"prompt"`
		Text        string          `json:"text"`
		Name        string          `json:"name"`
		From        string          `json:"from"`
		Mode        string          `json:"mode"`
		Steer       bool            `json:"steer"`
		Probe       bool            `json:"probe"`
		WaitSeconds int             `json:"wait_seconds"`
		AskID       string          `json:"ask_id"`
		Bot         string          `json:"bot"`
		Default     bool            `json:"default"`
		All         bool            `json:"all"`
		Behavior    string          `json:"behavior"`
		Message     string          `json:"message"`
		Updated     json.RawMessage `json:"updated_input"`
	}
	if len(args) > 0 && string(args) != "null" && json.Unmarshal(args, &in) != nil {
		return nil, errors.New("bad arguments")
	}
	short := func(s string, n int) bool { return len([]rune(s)) <= n && !strings.ContainsAny(s, "\x00\r") }
	if !short(in.Name, 64) || !short(in.From, 64) || len(in.Prompt) > maxQueueText || len(in.Text) > maxQueueText {
		return nil, errors.New("a value is too long")
	}
	switch strings.TrimPrefix(op, "claude-agent.") {
	case "projects":
		return m.opProjects(), nil
	case "next":
		items, err := m.Next(ctx, time.Duration(in.WaitSeconds)*time.Second)
		if err != nil {
			return nil, err
		}
		return map[string]any{"items": items}, nil
	case "decide":
		return map[string]any{"ok": true}, m.Decide(in.AskID, in.Behavior, in.Message, in.Updated)
	case "status":
		return m.opStatus(ctx, in.Probe), nil
	case "usage_mode":
		// The user's switch: the plugin offers it only on its panel, never as a Bot tool. One of: bot (one
		// Bot), default (the default for new Bots and the user's own sessions), all (every known Bot and
		// the default).
		ch := UsageChange{Mode: in.Mode, Bot: in.Bot, Default: in.Default, All: in.All}
		n := 0
		for _, set := range []bool{in.Bot != "", in.Default, in.All} {
			if set {
				n++
			}
		}
		switch {
		case !validMode(in.Mode):
			return nil, fmt.Errorf("unknown usage mode %q", clip(in.Mode, 32))
		case n != 1 || !okBot(in.Bot):
			return nil, errors.New("say which: one bot, the default, or all")
		case m.opt.SetUsage == nil:
			return nil, errors.New("the usage mode can't be changed here")
		}
		if err := m.opt.SetUsage(ch); err != nil {
			return nil, err
		}
		m.wake() // a raised limit may let the queue go
		return m.opStatus(ctx, false), nil
	case "limits":
		// The user's switch, like usage_mode: the plugin offers it only on its panel, never as a Bot tool. Raising
		// any cap is the user's decision. Each given value replaces the configured one.
		ch := LimitsChange{PerProfile: in.PerProfile, PerProject: in.PerProject, Total: in.Total}
		bad := func(v *int, min int) bool { return v != nil && (*v < min || *v > MaxLimit) }
		switch {
		case ch.PerProfile == nil && ch.PerProject == nil && ch.Total == nil:
			return nil, errors.New("say which limit: per_profile, per_project or total")
		case bad(ch.PerProfile, 1):
			return nil, fmt.Errorf("per_profile is a number from 1 to %d", MaxLimit)
		case bad(ch.PerProject, 0), bad(ch.Total, 0):
			return nil, fmt.Errorf("per_project and total are numbers from 0 (no cap) to %d", MaxLimit)
		case m.opt.SetLimits == nil:
			return nil, errors.New("the limits can't be changed here")
		}
		if err := m.opt.SetLimits(ch); err != nil {
			return nil, err
		}
		m.wake() // a raised limit may let the queue go
		return m.opStatus(ctx, false), nil
	case "sessions":
		return m.forward(ctx, in.Project, in.Role, "session.list", map[string]any{})
	case "messages":
		if !okID(in.ID) {
			return nil, errors.New("bad session id")
		}
		return m.forward(ctx, in.Project, in.Role, "session.messages", map[string]any{"id": in.ID, "limit": min(max(in.Limit, 0), 500)})
	case "start":
		if strings.TrimSpace(in.Prompt) == "" {
			return nil, errors.New("the prompt is empty")
		}
		mode, err := m.checkMode(in.Mode)
		if err != nil {
			return nil, err
		}
		if !okBot(in.Bot) {
			return nil, errors.New("bad bot id")
		}
		sel, err := m.selectRole(in.Project, in.Role)
		if err != nil {
			return nil, err
		}
		if sel.archived {
			return nil, archivedErr(sel.name, nil)
		}
		m.seeBot(in.Bot)
		return m.submit(ctx, QueueItem{Op: "start", Bot: in.Bot, Project: in.Project, Role: sel.id, Text: in.Prompt, Name: in.Name, From: in.From, Mode: mode})
	case "send":
		if !okID(in.ID) {
			return nil, errors.New("bad session id")
		}
		if strings.TrimSpace(in.Text) == "" {
			return nil, errors.New("the text is empty")
		}
		if !okBot(in.Bot) {
			return nil, errors.New("bad bot id")
		}
		sel, err := m.selectRole(in.Project, in.Role)
		if err != nil {
			return nil, err
		}
		m.seeBot(in.Bot)
		return m.submit(ctx, QueueItem{Op: "send", Bot: in.Bot, Project: in.Project, Role: sel.id, Session: in.ID, Text: in.Text, From: in.From, Steer: in.Steer})
	case "rename":
		if !okID(in.ID) || strings.TrimSpace(in.Name) == "" {
			return nil, errors.New("a session id and a name are needed")
		}
		return m.forward(ctx, in.Project, in.Role, "session.rename", map[string]any{"id": in.ID, "name": in.Name})
	case "fork":
		if !okID(in.ID) {
			return nil, errors.New("bad session id")
		}
		return m.forward(ctx, in.Project, in.Role, "session.fork", map[string]any{"id": in.ID, "name": in.Name})
	case "interrupt":
		if !okID(in.ID) {
			return nil, errors.New("bad session id")
		}
		return m.forward(ctx, in.Project, in.Role, "session.interrupt", map[string]any{"id": in.ID})
	case "close":
		if !okID(in.ID) {
			return nil, errors.New("bad session id")
		}
		res, err := m.forward(ctx, in.Project, in.Role, "session.close", map[string]any{"id": in.ID})
		if err == nil {
			m.mu.Lock()
			if sl := m.exactLocked(m.roleKey(in.Project, in.Role), in.ID); sl != nil {
				m.releaseLocked(sl)
			}
			m.mu.Unlock()
		}
		return res, err
	case "mode":
		mode, err := m.checkMode(in.Mode)
		if err != nil || !okID(in.ID) || in.Mode == "" {
			return nil, errors.New("a session id and a valid mode are needed")
		}
		return m.forward(ctx, in.Project, in.Role, "session.mode", map[string]any{"id": in.ID, "mode": mode})
	}
	return nil, fmt.Errorf("Rubi Home doesn't do %q", op)
}

func (m *Manager) checkMode(s string) (string, error) {
	if s == "" {
		return "default", nil
	}
	if !modes[s] {
		return "", fmt.Errorf("unknown mode %q", clip(s, 32))
	}
	if (s == "bypassPermissions" || s == "dontAsk") && m.opt.AllowBypass != nil && !m.opt.AllowBypass() {
		return "", fmt.Errorf("the mode %s is switched off on this computer (agent_allow_bypass)", s)
	}
	return s, nil
}

// roleKey is the key of the slots of the role an operation names (a bad role names no slot).
func (m *Manager) roleKey(project, role string) string {
	if sel, err := m.selectRole(project, role); err == nil && sel.id != "" {
		return sel.id
	}
	return project
}

func archivedErr(name string, err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("role %s is archived; it can't start sessions", clip(name, 40))
}

func (m *Manager) forward(ctx context.Context, project, role, method string, params any) (any, error) {
	p, err := m.projectFor(project, role)
	if err != nil {
		return nil, err
	}
	r, err := m.ensure(ctx, p)
	if err != nil {
		return nil, err
	}
	var res json.RawMessage
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := r.Call(cctx, method, params, &res); err != nil {
		return nil, err
	}
	if len(res) == 0 {
		res = json.RawMessage(`{}`)
	}
	return res, nil
}

func (m *Manager) opProjects() any {
	type row struct {
		ID       string `json:"id"`
		Dir      string `json:"dir"`
		Role     string `json:"role"`      // the role's id (the legacy role's is the project id)
		RoleName string `json:"role_name"` // "default" for the legacy role
		VMState  State  `json:"vm_state"`
		SlotBusy bool   `json:"slot_busy"`
		Queue    int    `json:"queue"`
		Running  int    `json:"sessions_running"`
	}
	queued := map[string]int{}
	qroles := map[string]map[string]bool{} // project -> role ids with something queued
	for _, it := range m.q.list() {
		queued[it.rk()]++
		if it.Role != "" {
			if qroles[it.Project] == nil {
				qroles[it.Project] = map[string]bool{}
			}
			qroles[it.Project][it.Role] = true
		}
	}
	per := m.usageNow().For("").PerProfile // once, outside the lock
	rows := []row{}
	for _, p := range m.projects() {
		// One row for the legacy role, then one for each isolated role that has a VM, a session or a queued item.
		keys := []string{p.ID}
		m.mu.Lock()
		extra := map[string]bool{}
		for k, ps := range m.projs {
			if ps.ID == p.ID && ps.Role != "" && ps.Dir == p.Dir {
				extra[k] = true
			}
		}
		for _, s := range m.slots {
			if s.project == p.ID && s.role != p.ID {
				extra[s.role] = true
			}
		}
		m.mu.Unlock()
		for id := range qroles[p.ID] {
			extra[id] = true
		}
		var ids []string
		for id := range extra {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return m.roleName(p.ID, ids[i]) < m.roleName(p.ID, ids[j]) })
		keys = append(keys, ids...)
		for _, k := range keys {
			r := row{ID: p.ID, Dir: p.Dir, Role: k, RoleName: m.roleName(p.ID, k), VMState: StateStopped, Queue: queued[k]}
			m.mu.Lock()
			if ps := m.projs[k]; ps != nil && ps.Dir == p.Dir && ps.vm != nil {
				r.VMState = ps.vm.State()
			}
			for _, s := range m.slots {
				if s.role == k {
					r.Running++
				}
			}
			r.SlotBusy = r.Running >= per // the role's slots are full
			m.mu.Unlock()
			rows = append(rows, r)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return map[string]any{"projects": rows}
}

func (m *Manager) opStatus(ctx context.Context, probe bool) any {
	if probe {
		m.probe(ctx)
	}
	uc := m.usageNow() // once, outside the lock
	m.mu.Lock()
	slots := []map[string]any{}
	for _, s := range m.slots {
		slots = append(slots, map[string]any{"project": s.project, "role": s.role, "role_name": m.roleName(s.project, s.role), "session": s.session, "busy": true, "background": len(s.bg), "subagents": s.sub, "bot": s.bot})
	}
	slot := map[string]any{"busy": false, "project": "", "session": "", "background": 0} // for older plugins: the first
	if len(slots) > 0 {
		slot = slots[0]
	}
	def := uc.For("")
	bots := m.botRowsLocked(uc)
	reasons := map[string]string{}
	waiting := m.q.list()
	for _, it := range waiting {
		r, _ := m.reasonLocked(uc, it)
		if r == "" {
			r = "waiting: earlier messages are queued first"
		}
		reasons[it.ID] = r
	}
	login := m.login
	usage := m.usage
	m.mu.Unlock()
	queue := []map[string]any{}
	for i, it := range waiting {
		queue = append(queue, map[string]any{"id": it.ID, "op": it.Op, "project": it.Project, "role": it.rk(), "role_name": m.roleName(it.Project, it.rk()), "session": it.Session,
			"from": it.From, "at": it.At, "position": i + 1, "reason": reasons[it.ID]})
	}
	if len(login) == 0 {
		login = json.RawMessage("null")
	}
	return map[string]any{"login": login, "slot": slot, "slots": slots,
		"limits":           map[string]any{"per_profile": def.PerProfile, "per_project": def.PerProject, "total": uc.Machine()},
		"usage":            map[string]any{"default": def.Mode, "modes": UsageModes, "bots": bots},
		"sessions_running": len(slots), "queue": queue, "usage_today": usage}
}

// botRowsLocked is one row per Bot known (listed in the configuration or running something), with its mode,
// its limits and what it runs now. The user's own sessions (id "") show when they run.
func (m *Manager) botRowsLocked(c UsageConfig) []map[string]any {
	ids := map[string]bool{}
	for id := range c.Bots {
		ids[id] = true
	}
	running := map[string]int{}
	for _, s := range m.slots {
		ids[s.bot] = true
		running[s.bot]++
	}
	var sorted []string
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	rows := []map[string]any{}
	for _, id := range sorted {
		l := c.For(id)
		rows = append(rows, map[string]any{"id": id, "mode": l.Mode, "custom": l.Custom, "sessions_running": running[id],
			"sessions_limit": l.Sessions, "subagents_per_session": l.Subagents})
	}
	return rows
}

// probe asks a running runner for the login state (usage.probe spends no tokens); it never starts a VM.
func (m *Manager) probe(ctx context.Context) {
	m.mu.Lock()
	var r *Runner
	for _, p := range m.projs {
		if p.runner != nil {
			r = p.runner
			break
		}
	}
	m.mu.Unlock()
	if r == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var res json.RawMessage
	if r.Call(cctx, "usage.probe", nil, &res) == nil {
		m.mu.Lock()
		m.login = res
		m.mu.Unlock()
	}
}

// netDenyFor is how long a refused net.ask is remembered, so a program that retries doesn't prompt again.
var netDenyFor = 5 * time.Minute

type netFlight struct {
	done chan struct{}
	ok   bool
}

// askHost is the proxy's question about a host that isn't on the list: a net.ask item the plugin
// answers with decide. An allow is recorded for the project (agent-net.json) and the proxy adds it. One
// question per host at a time; a denial is remembered for a few minutes.
func (m *Manager) askHost(ctx context.Context, p *projState, host string) bool {
	key := p.key() + "|" + host
	m.mu.Lock()
	if t, ok := m.netDenied[key]; ok && m.opt.Now().Sub(t) < netDenyFor {
		m.mu.Unlock()
		return false
	}
	if f := m.netFlights[key]; f != nil {
		m.mu.Unlock()
		select {
		case <-f.done:
			return f.ok
		case <-ctx.Done():
			return false
		}
	}
	f := &netFlight{done: make(chan struct{})}
	if m.netFlights == nil {
		m.netFlights = map[string]*netFlight{}
		m.netDenied = map[string]time.Time{}
	}
	m.netFlights[key] = f
	m.mu.Unlock()
	r := m.waitDecision(ctx, p.ID, Item{Type: "net.ask", Project: p.ID, Role: p.key(), Host: host, Op: "connect"})
	f.ok = r.Behavior == "allow" && m.net.add(p.netKey(), host)
	m.mu.Lock()
	delete(m.netFlights, key)
	if !f.ok && len(m.netDenied) < 1024 {
		m.netDenied[key] = m.opt.Now()
	}
	m.mu.Unlock()
	close(f.done)
	return f.ok
}

// SlotIdleTTL is how long a session may hold a slot without a sign of life (no event, no ask) before it
// is interrupted and the slot freed, so a wedged runner can't block the queue forever.
var SlotIdleTTL = 30 * time.Minute

func (m *Manager) checkSlot() {
	type stale struct {
		project, session string
		r                *Runner
	}
	m.mu.Lock()
	var out []stale
	for _, s := range append([]*slotState(nil), m.slots...) {
		if s.last.IsZero() || m.opt.Now().Sub(s.last) < SlotIdleTTL {
			continue
		}
		// A session waiting for the user's answer is not idle. A start still being handed to the runner (no session
		// id yet) gets the time that takes on top.
		if m.askOpenLocked(s) || s.session == "" && m.opt.Now().Sub(s.last) < SlotIdleTTL+startTimeout {
			continue
		}
		var r *Runner
		if p := m.projs[s.role]; p != nil {
			r = p.runner
		}
		out = append(out, stale{s.project, s.session, r})
		m.releaseLocked(s)
		m.pushLocked(Item{Type: "event", Project: s.project, Role: s.role, SessionID: s.session,
			Event: json.RawMessage(`{"kind":"status","status":"closed","reason":"no activity; the slot was released"}`)})
	}
	m.mu.Unlock()
	for _, st := range out {
		log.Printf("agent: session %q of project %q held a slot with no activity for %v; interrupted and released", clip(st.session, 64), clip(st.project, 64), SlotIdleTTL)
		if st.r != nil && st.session != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = st.r.Call(ctx, "session.interrupt", map[string]any{"id": st.session}, nil)
			cancel()
		}
	}
}

// askOpenLocked says whether a question of the slot's session waits for the user (an ask of the project that
// names no session counts for all of its slots).
func (m *Manager) askOpenLocked(s *slotState) bool {
	for _, a := range m.asks {
		if a.role == s.role && (a.item.SessionID == "" || a.item.SessionID == s.session) {
			return true
		}
	}
	return false
}

// prepareWorkDir makes /work/.rubi with a .gitignore of "*" on the first start, so the runner's sessions and
// config (which can hold conversation text and tokens) are never committed. /work is shared with the VM
// and the agent controls its content: nothing is followed through a symlink, and only a real directory and a
// regular file are accepted. Nothing else on the Mac reads /work for decisions.
func prepareWorkDir(dir string) error {
	rubi := filepath.Join(dir, ".rubi")
	st, err := os.Lstat(rubi)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(rubi, 0o755); err != nil {
			return err
		}
	case err != nil:
		return err
	case !st.IsDir():
		return fmt.Errorf("%s isn't a plain directory; refusing to start", rubi)
	}
	f, err := os.OpenFile(filepath.Join(rubi, ".gitignore"), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("*\n")
	return err
}
