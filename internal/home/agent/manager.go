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
)

// Options configures a Manager.
type Options struct {
	// Dir is the helper's directory (the queue file lives there) and Protected more directories no
	// project may contain or be (the user's home, by default).
	Dir       string
	Protected []string
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
	// Wait is how long an ask waits (DefaultWait when zero).
	Wait time.Duration
	// ProxyListen and ProxyVMHost: where the proxy listens and the host name the VM uses to reach it
	// (the listen host when empty).
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
	QueueID   string          `json:"queue_id,omitempty"`
	Error     string          `json:"error,omitempty"`
	At        time.Time       `json:"at"`
}

type slotState struct {
	busy    bool
	project string
	session string
	turn    bool            // a turn is running
	last    time.Time       // the last sign of life (event, ask, delivery)
	bg      map[string]bool // background subagents still running
}

type pendingAsk struct {
	item      Item
	project   string
	delivered bool
	answer    chan AskResult
}

type projState struct {
	Project
	startMu sync.Mutex // one start at a time
	vm      VM
	runner  *Runner // guarded by Manager.mu
	proxy   *Proxy
	hosts   []string
}

// extraHosts are the hosts besides the base and git hosts: the owner's list, the forges, and what the
// owner approved for this project.
func (m *Manager) extraHosts(p *projState) []string {
	var out []string
	if m.opt.AllowedHosts != nil {
		out = append(out, m.opt.AllowedHosts()...)
	}
	if m.opt.NoDefaultForges == nil || !m.opt.NoDefaultForges() {
		out = append(out, ForgeHosts...)
	}
	return append(out, m.net.hosts(p.Dir)...)
}

// Manager is the Mac side of the agent: projects, the slot, the queue, asks.
type Manager struct {
	opt Options
	q   *diskQueue
	net *netState

	mu       sync.Mutex
	projs    map[string]*projState
	slot     slotState
	asks     map[string]*pendingAsk
	out      []Item
	signal   chan struct{}
	polls    int
	lastPoll time.Time
	login    json.RawMessage
	usage    usageDay
	lastCost map[string]float64
	closed   bool

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
	m := &Manager{opt: opt, projs: map[string]*projState{}, asks: map[string]*pendingAsk{}, lastCost: map[string]float64{},
		kick: make(chan struct{}, 1), stop: make(chan struct{})}
	m.q = &diskQueue{}
	if opt.Dir != "" {
		m.q.path = filepath.Join(opt.Dir, QueueFile)
	}
	m.q.load()
	m.net = newNetState(opt.Dir)
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
	px := p.proxy
	p.proxy = nil
	m.mu.Unlock()
	if r != nil {
		_ = r.Close()
	}
	if px != nil {
		px.Close()
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
	for _, p := range append([]string{m.opt.Dir}, m.opt.Protected...) {
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

func (m *Manager) project(id string) (*projState, error) {
	for _, p := range m.projects() {
		if p.ID == id {
			m.mu.Lock()
			defer m.mu.Unlock()
			if ps := m.projs[id]; ps != nil && ps.Dir == p.Dir {
				return ps, nil
			}
			if m.opt.NewVM == nil {
				return nil, errors.New("no VM is configured")
			}
			ps := &projState{Project: p, vm: m.opt.NewVM(p)}
			m.projs[id] = ps
			return ps, nil
		}
	}
	return nil, fmt.Errorf("unknown project %q", clip(id, 64))
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
			return r, nil
		}
	}
	if err := prepareWorkDir(p.Dir); err != nil {
		return nil, err
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
	hosts := AllowList(m.opt.GitHosts(), m.extraHosts(p))
	px, err := StartProxy(ProxyConfig{Listen: m.opt.ProxyListen, AuthToken: proxyToken, Hosts: hosts, Exempt: m.opt.GitHosts(),
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
	env := map[string]string{"RUBI_RUN_TOKEN": token, "HTTPS_PROXY": proxyURL, "https_proxy": proxyURL,
		"GIT_TERMINAL_PROMPT": "0"}
	conn, err := p.vm.Start(sctx, StartSpec{Project: p.Project, Env: env})
	if err != nil {
		px.Close()
		return nil, fmt.Errorf("starting the VM: %w", err)
	}
	r, err = Connect(sctx, conn, token, &handler{m: m, p: p})
	if err != nil {
		px.Close()
		_ = p.vm.Stop(sctx)
		return nil, err
	}
	m.mu.Lock()
	p.runner, p.proxy, p.hosts = r, px, hosts
	m.mu.Unlock()
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
	px := p.proxy
	p.proxy = nil
	if m.slot.busy && m.slot.project == p.ID {
		m.slot = slotState{}
		m.pushLocked(Item{Type: "event", Project: p.ID, Event: json.RawMessage(`{"kind":"status","status":"closed","reason":"runner ended"}`)})
	}
	m.mu.Unlock()
	if px != nil {
		px.Close()
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
	pa := &pendingAsk{item: it, project: project, answer: make(chan AskResult, 1)}
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
	cancel := h.m.cancels[h.p.ID+"|"+askID]
	h.m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (h *handler) Ask(ctx context.Context, a AskParams) AskResult {
	// The runner gives a call up with perm.cancel {ask_id}: that ends the wait and drops the item.
	ctx, cancel := context.WithCancel(ctx)
	key := h.p.ID + "|" + a.AskID
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
	it := Item{Type: "perm.ask", Project: h.p.ID, SessionID: a.SessionID, AgentID: a.AgentID, AgentType: a.AgentType,
		Tool: a.Tool, ToolUseID: a.ToolUseID, Via: a.Via, Input: a.Input, Cwd: a.Cwd, Mode: a.Mode}
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
		it := Item{Type: "cred.ask", Project: p.ID, Host: host, Path: clip(strings.TrimSpace(kv["path"]), 200), Op: "push"}
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

// ---- events and the slot ----

func (m *Manager) ownerLocked(p *projState, session string) bool {
	return m.slot.busy && m.slot.project == p.ID && (m.slot.session == "" || m.slot.session == session || session == "")
}

func (m *Manager) releaseLocked() {
	m.slot = slotState{}
	m.wake()
}

func (m *Manager) onEvent(p *projState, ev Event) {
	var f struct {
		Status string `json:"status"`
		State  string `json:"state"`
		Tasks  *[]struct {
			ID string `json:"task_id"`
		} `json:"tasks"`
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ownerLocked(p, ev.SessionID) {
		m.slot.last = m.opt.Now()
	}
	switch ev.Kind {
	case "status":
		switch f.Status {
		case "busy":
			if !m.slot.busy {
				m.slot = slotState{busy: true, project: p.ID, session: ev.SessionID, turn: true, last: m.opt.Now()}
			} else if m.ownerLocked(p, ev.SessionID) {
				m.slot.turn = true
				if m.slot.session == "" {
					m.slot.session = ev.SessionID
				}
			}
		case "idle":
			if m.ownerLocked(p, ev.SessionID) {
				m.slot.turn = false
				m.maybeReleaseLocked()
			}
		case "closed":
			if m.ownerLocked(p, ev.SessionID) {
				m.releaseLocked()
			}
		}
	case "background":
		if m.ownerLocked(p, ev.SessionID) && f.Tasks != nil {
			// The runner lists every task still alive: that is the whole set.
			m.slot.bg = map[string]bool{}
			for i, t := range *f.Tasks {
				if t.ID != "" && i < 1024 {
					m.slot.bg[t.ID] = true
				}
			}
			m.maybeReleaseLocked()
		} else if m.ownerLocked(p, ev.SessionID) && ev.AgentID != "" {
			if ev.Done {
				delete(m.slot.bg, ev.AgentID)
				m.maybeReleaseLocked()
			} else {
				if m.slot.bg == nil {
					m.slot.bg = map[string]bool{}
				}
				if len(m.slot.bg) < 1024 {
					m.slot.bg[ev.AgentID] = true
				}
			}
		}
	case "result":
		if ev.AgentID != "" {
			if m.ownerLocked(p, ev.SessionID) {
				delete(m.slot.bg, ev.AgentID)
				m.maybeReleaseLocked()
			}
		} else {
			m.addUsageLocked(ev.SessionID, f.Usage.In, f.Usage.Out, f.TotalCost)
		}
	}
	raw := ev.Raw
	if len(raw) > maxItemBytes {
		raw, _ = json.Marshal(map[string]any{"kind": ev.Kind, "session_id": ev.SessionID, "truncated": true})
	}
	m.pushLocked(Item{Type: "event", Project: p.ID, SessionID: ev.SessionID, AgentID: ev.AgentID, Event: raw})
}

// maybeReleaseLocked frees the slot when no turn runs and no background subagent is left.
func (m *Manager) maybeReleaseLocked() {
	if m.slot.busy && !m.slot.turn && len(m.slot.bg) == 0 {
		m.releaseLocked()
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
	n := Item{Type: "queue", Action: action, QueueID: it.ID, Project: it.Project, SessionID: it.Session, Op: it.Op}
	if extra != nil {
		extra(&n)
	}
	m.mu.Lock()
	m.pushLocked(n)
	m.mu.Unlock()
}

// submit delivers a start or send now when the slot allows, otherwise queues it on disk. The result is
// what the op returns.
func (m *Manager) submit(ctx context.Context, it QueueItem) (map[string]any, error) {
	p, err := m.project(it.Project)
	if err != nil {
		return nil, err
	}
	m.ensureLoop()
	it.ID, it.At = newQueueID(), m.opt.Now().UTC()
	m.mu.Lock()
	owner := it.Op == "send" && m.slot.busy && m.slot.project == it.Project && m.slot.session == it.Session
	acquire := !m.slot.busy && m.q.len() == 0
	switch {
	case owner:
		m.slot.turn = true
	case acquire:
		m.slot = slotState{busy: true, project: it.Project, session: it.Session, turn: true, last: m.opt.Now()}
	}
	m.mu.Unlock()
	if owner || acquire {
		res, err := m.deliver(ctx, p, it)
		if err != nil {
			if acquire {
				m.mu.Lock()
				m.releaseLocked()
				m.mu.Unlock()
			}
			return nil, err
		}
		if it.Op == "send" {
			res["queued"], res["position"], res["delivered"] = true, 0, true
		}
		return res, nil
	}
	pos, err := m.q.add(it)
	if err != nil {
		return nil, err
	}
	m.emitQueue("queued", it, nil)
	return map[string]any{"queued": true, "position": pos, "delivered": false}, nil
}

func (m *Manager) deliver(ctx context.Context, p *projState, it QueueItem) (map[string]any, error) {
	r, err := m.ensure(ctx, p)
	if err != nil {
		return nil, err
	}
	switch it.Op {
	case "start":
		var res struct {
			ID string `json:"id"`
		}
		err := r.Call(ctx, "session.start", map[string]any{"prompt": it.Text, "name": it.Name, "from": it.From, "mode": it.Mode}, &res)
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		if m.slot.busy && m.slot.project == p.ID && m.slot.session == "" {
			m.slot.session = res.ID
		}
		m.mu.Unlock()
		return map[string]any{"id": res.ID}, nil
	default:
		var res json.RawMessage
		err := r.Call(ctx, "session.send", map[string]any{"id": it.Session, "text": it.Text, "from": it.From, "steer": it.Steer}, &res)
		if err != nil {
			return nil, err
		}
		return map[string]any{}, nil
	}
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
		case <-m.stop:
			return
		}
	}
}

// drain delivers queued items, in order, while the slot is free.
func (m *Manager) drain() {
	for {
		for _, it := range m.q.sweep(m.opt.Now()) {
			m.emitQueue("expired", it, nil)
		}
		m.mu.Lock()
		if m.closed || m.slot.busy {
			m.mu.Unlock()
			return
		}
		it, ok := m.q.pop()
		if !ok {
			m.mu.Unlock()
			return
		}
		m.slot = slotState{busy: true, project: it.Project, session: it.Session, turn: true, last: m.opt.Now()}
		m.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
		var err error
		p, perr := m.project(it.Project)
		if perr != nil {
			err = perr
		} else {
			_, err = m.deliver(ctx, p, it)
		}
		cancel()
		if err != nil {
			m.mu.Lock()
			m.releaseLocked()
			m.mu.Unlock()
			m.emitQueue("failed", it, func(n *Item) { n.Error = clip(err.Error(), 300) })
			continue
		}
		m.emitQueue("delivered", it, nil)
	}
}

// ---- operations ----

var modes = map[string]bool{"default": true, "acceptEdits": true, "plan": true, "bypassPermissions": true, "dontAsk": true}

func okID(s string) bool { return idRE.MatchString(s) }

// Handle serves one claude-agent.* operation.
func (m *Manager) Handle(ctx context.Context, op string, args json.RawMessage) (any, error) {
	var in struct {
		Project     string          `json:"project"`
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
	case "sessions":
		return m.forward(ctx, in.Project, "session.list", map[string]any{})
	case "messages":
		if !okID(in.ID) {
			return nil, errors.New("bad session id")
		}
		return m.forward(ctx, in.Project, "session.messages", map[string]any{"id": in.ID, "limit": min(max(in.Limit, 0), 500)})
	case "start":
		if strings.TrimSpace(in.Prompt) == "" {
			return nil, errors.New("the prompt is empty")
		}
		mode, err := m.checkMode(in.Mode)
		if err != nil {
			return nil, err
		}
		return m.submit(ctx, QueueItem{Op: "start", Project: in.Project, Text: in.Prompt, Name: in.Name, From: in.From, Mode: mode})
	case "send":
		if !okID(in.ID) {
			return nil, errors.New("bad session id")
		}
		if strings.TrimSpace(in.Text) == "" {
			return nil, errors.New("the text is empty")
		}
		return m.submit(ctx, QueueItem{Op: "send", Project: in.Project, Session: in.ID, Text: in.Text, From: in.From, Steer: in.Steer})
	case "rename":
		if !okID(in.ID) || strings.TrimSpace(in.Name) == "" {
			return nil, errors.New("a session id and a name are needed")
		}
		return m.forward(ctx, in.Project, "session.rename", map[string]any{"id": in.ID, "name": in.Name})
	case "fork":
		if !okID(in.ID) {
			return nil, errors.New("bad session id")
		}
		return m.forward(ctx, in.Project, "session.fork", map[string]any{"id": in.ID, "name": in.Name})
	case "interrupt":
		if !okID(in.ID) {
			return nil, errors.New("bad session id")
		}
		return m.forward(ctx, in.Project, "session.interrupt", map[string]any{"id": in.ID})
	case "close":
		if !okID(in.ID) {
			return nil, errors.New("bad session id")
		}
		res, err := m.forward(ctx, in.Project, "session.close", map[string]any{"id": in.ID})
		if err == nil {
			m.mu.Lock()
			if m.slot.busy && m.slot.project == in.Project && m.slot.session == in.ID {
				m.releaseLocked()
			}
			m.mu.Unlock()
		}
		return res, err
	case "mode":
		mode, err := m.checkMode(in.Mode)
		if err != nil || !okID(in.ID) || in.Mode == "" {
			return nil, errors.New("a session id and a valid mode are needed")
		}
		return m.forward(ctx, in.Project, "session.mode", map[string]any{"id": in.ID, "mode": mode})
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

func (m *Manager) forward(ctx context.Context, project, method string, params any) (any, error) {
	p, err := m.project(project)
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
		VMState  State  `json:"vm_state"`
		SlotBusy bool   `json:"slot_busy"`
		Queue    int    `json:"queue"`
	}
	queued := map[string]int{}
	for _, it := range m.q.list() {
		queued[it.Project]++
	}
	rows := []row{}
	for _, p := range m.projects() {
		r := row{ID: p.ID, Dir: p.Dir, VMState: StateStopped, Queue: queued[p.ID]}
		m.mu.Lock()
		if ps := m.projs[p.ID]; ps != nil && ps.Dir == p.Dir && ps.vm != nil {
			r.VMState = ps.vm.State()
		}
		r.SlotBusy = m.slot.busy && m.slot.project == p.ID
		m.mu.Unlock()
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return map[string]any{"projects": rows}
}

func (m *Manager) opStatus(ctx context.Context, probe bool) any {
	if probe {
		m.probe(ctx)
	}
	m.mu.Lock()
	slot := map[string]any{"busy": m.slot.busy, "project": m.slot.project, "session": m.slot.session,
		"background": len(m.slot.bg)}
	login := m.login
	usage := m.usage
	m.mu.Unlock()
	queue := []map[string]any{}
	for i, it := range m.q.list() {
		queue = append(queue, map[string]any{"id": it.ID, "op": it.Op, "project": it.Project, "session": it.Session,
			"from": it.From, "at": it.At, "position": i + 1})
	}
	if len(login) == 0 {
		login = json.RawMessage("null")
	}
	return map[string]any{"login": login, "slot": slot, "queue": queue, "usage_today": usage}
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
	key := p.ID + "|" + host
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
	r := m.waitDecision(ctx, p.ID, Item{Type: "net.ask", Project: p.ID, Host: host, Op: "connect"})
	f.ok = r.Behavior == "allow" && m.net.add(p.Dir, host)
	m.mu.Lock()
	delete(m.netFlights, key)
	if !f.ok && len(m.netDenied) < 1024 {
		m.netDenied[key] = m.opt.Now()
	}
	m.mu.Unlock()
	close(f.done)
	return f.ok
}

// SlotIdleTTL is how long a session may hold the slot without a sign of life (no event, no ask) before it
// is interrupted and the slot freed, so a wedged runner can't block the queue forever.
var SlotIdleTTL = 30 * time.Minute

func (m *Manager) checkSlot() {
	m.mu.Lock()
	if !m.slot.busy || m.slot.last.IsZero() || m.opt.Now().Sub(m.slot.last) < SlotIdleTTL || len(m.asks) > 0 {
		m.mu.Unlock()
		return
	}
	project, session := m.slot.project, m.slot.session
	var r *Runner
	if p := m.projs[project]; p != nil {
		r = p.runner
	}
	m.releaseLocked()
	m.pushLocked(Item{Type: "event", Project: project, SessionID: session,
		Event: json.RawMessage(`{"kind":"status","status":"closed","reason":"no activity; the slot was released"}`)})
	m.mu.Unlock()
	log.Printf("agent: session %q of project %q held the slot with no activity for %v; interrupted and released", clip(session, 64), clip(project, 64), SlotIdleTTL)
	if r != nil && session != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = r.Call(ctx, "session.interrupt", map[string]any{"id": session}, nil)
	}
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
