package sshtarget

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/sshtarget/sshtest"
)

type rig struct {
	t     *testing.T
	dir   string
	srv   *sshtest.Server
	store *Store
	hub   *Hub
	// to is where every dial goes (swappable: a host key change is another server answering).
	to      atomic.Value // string
	granted atomic.Bool
	notes   *notes
}

type notes struct {
	mu   sync.Mutex
	list []string
}

func (n *notes) add(method string, params any) {
	b, _ := json.Marshal(map[string]any{"method": method, "params": params})
	n.mu.Lock()
	n.list = append(n.list, string(b))
	n.mu.Unlock()
}

func (n *notes) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.list...)
}

func (n *notes) of(method string) []map[string]any {
	var out []map[string]any
	for _, s := range n.all() {
		var m struct {
			Method string
			Params map[string]any
		}
		if json.Unmarshal([]byte(s), &m) == nil && m.Method == method {
			out = append(out, m.Params)
		}
	}
	return out
}

// output is everything the stream wrote to fd (1 or 2), decoded.
func (n *notes) output(fd int) string {
	var sb strings.Builder
	for _, p := range n.of("ssh.data") {
		if int(p["fd"].(float64)) == fd {
			b, _ := base64.StdEncoding.DecodeString(p["b64"].(string))
			sb.Write(b)
		}
	}
	return sb.String()
}

func newRig(t *testing.T, lim Limits) *rig {
	t.Helper()
	srv, err := sshtest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	r := &rig{t: t, dir: filepath.Join(t.TempDir(), DirName), srv: srv, notes: &notes{}}
	r.to.Store(srv.Addr())
	r.granted.Store(true)
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, r.to.Load().(string))
	}
	r.store, err = Open(r.dir, dial, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.hub = NewHub(Config{Store: r.store, Audit: NewAudit(r.dir), Limits: lim,
		Granted: func(role, target string) bool { return r.granted.Load() }})
	t.Cleanup(r.hub.CloseAll)
	return r
}

func (r *rig) register(name string, cmds []string, ask bool) string {
	r.t.Helper()
	line, err := r.store.Register(context.Background(), RegisterArgs{Name: name, Host: "web-1.example.com", Port: 22, User: "deploy",
		Fingerprint: r.srv.Fingerprint(), AllowedCommands: cmds, AskEachTime: ask})
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.srv.Authorize(line); err != nil {
		r.t.Fatal(err)
	}
	return line
}

func (r *rig) open(cmd string) (*Stream, error) {
	s, err := r.hub.Open(context.Background(), OpenReq{Role: "role-a", Project: "my-project", Bot: "bot-1", Target: "web-1", Command: cmd}, r.notes.add)
	if err == nil {
		s.Start()
	}
	return s, err
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", what)
}

func TestRegisterRefusesAWrongFingerprint(t *testing.T) {
	r := newRig(t, Limits{})
	_, err := r.store.Register(context.Background(), RegisterArgs{Name: "web-1", Host: "web-1.example.com", User: "deploy",
		Fingerprint: "SHA256:" + strings.Repeat("A", 43)})
	if err == nil || !strings.Contains(err.Error(), "not the fingerprint given") {
		t.Fatalf("%v", err)
	}
	if _, ok := r.store.Get("web-1"); ok {
		t.Fatal("registered")
	}
	if _, err := os.Stat(filepath.Join(r.dir, "keys", "web-1")); err == nil {
		t.Fatal("a key was written")
	}
}

func TestRegisterPinsAndGeneratesAPrivateKey(t *testing.T) {
	r := newRig(t, Limits{})
	line := r.register("web-1", []string{"systemctl status nginx"}, false)
	pub, _, opts, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || pub.Type() != ssh.KeyAlgoED25519 {
		t.Fatalf("%v %v", err, pub)
	}
	if !strings.HasPrefix(line, `restrict,command="systemctl status nginx" ssh-ed25519 `) {
		t.Fatal(line)
	}
	_ = opts
	for p, want := range map[string]os.FileMode{r.dir: 0o700, filepath.Join(r.dir, "keys"): 0o700, filepath.Join(r.dir, "keys", "web-1"): 0o600} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != want {
			t.Fatalf("%s: %v %v", p, st, err)
		}
	}
	tg, _ := r.store.Get("web-1")
	if tg.Fingerprint != r.srv.Fingerprint() || tg.Port != 22 {
		t.Fatalf("%+v", tg)
	}
	// Two commands, or a prefix pattern: restrict only.
	r2 := newRig(t, Limits{})
	if l := r2.register("web-1", []string{"a", "b"}, false); !strings.HasPrefix(l, "restrict ssh-ed25519 ") {
		t.Fatal(l)
	}
	r3 := newRig(t, Limits{})
	if l := r3.register("web-1", []string{"tail -n *"}, false); !strings.HasPrefix(l, "restrict ssh-ed25519 ") {
		t.Fatal(l)
	}
	// It survives a restart.
	s2, err := Open(r.dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := s2.List(); len(v) != 1 || v[0].KeyType != "ssh-ed25519" || v[0].Name != "web-1" {
		t.Fatalf("%+v", v)
	}
	if _, err := r.store.Register(context.Background(), RegisterArgs{Name: "web-1", Host: "h.example.com", User: "u", Fingerprint: r.srv.Fingerprint()}); err == nil {
		t.Fatal("registered twice")
	}
	if err := r.store.Remove("web-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "keys", "web-1")); err == nil {
		t.Fatal("the key stays")
	}
}

func TestScan(t *testing.T) {
	r := newRig(t, Limits{})
	res, err := r.store.Scan(context.Background(), "web-1.example.com", 22)
	if err != nil || res.Fingerprint != r.srv.Fingerprint() || res.KeyType != "ssh-ed25519" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := r.store.Scan(context.Background(), "-oProxyCommand=x", 22); err == nil {
		t.Fatal("bad host accepted")
	}
	if _, err := r.store.Scan(context.Background(), "h.example.com", 70000); err == nil {
		t.Fatal("bad port accepted")
	}
}

func TestAllows(t *testing.T) {
	tg := Target{AllowedCommands: []string{"uptime", "systemctl status *", "tail -n 50 /var/log/"}}
	for cmd, want := range map[string]bool{
		"uptime": true, "uptime; id": false, "systemctl status nginx": true, "systemctl status nginx; rm -rf /": false,
		"systemctl status $(id)": false, "systemctl status a && b": false, "systemctl restart nginx": false,
		"systemctl status nginx\nid": false, "tail -n 50 /var/log/": true, "": false,
	} {
		if got := tg.Allows(cmd); got != want {
			t.Errorf("%q: %v", cmd, got)
		}
	}
	if !(Target{}).Allows("anything; goes") {
		t.Error("no patterns means any command")
	}
}

func TestRunsACommandRelaysAndAudits(t *testing.T) {
	r := newRig(t, Limits{})
	r.srv.Handler = func(cmd string, in io.Reader, out, errw io.Writer) int {
		b, _ := io.ReadAll(in)
		fmt.Fprintf(out, "out:%s:%s", cmd, b)
		fmt.Fprint(errw, "warn")
		return 3
	}
	r.register("web-1", nil, false)
	s, err := r.open("cat file --password=hunter2")
	if err != nil {
		t.Fatal(err)
	}
	s.h.Data("role-a", s.ID(), 0, []byte("hello"), true)
	eventually(t, "exit", func() bool { return len(r.notes.of("ssh.exit")) == 1 })
	ex := r.notes.of("ssh.exit")[0]
	if ex["code"].(float64) != 3 || ex["stream"] != s.ID() {
		t.Fatalf("%v", ex)
	}
	if got := r.notes.output(1); !strings.Contains(got, ":hello") {
		t.Fatalf("%q", got)
	}
	if r.notes.output(2) != "warn" {
		t.Fatalf("%q", r.notes.output(2))
	}
	if acks := r.notes.of("ssh.ack"); len(acks) != 1 || acks[0]["n"].(float64) != 5 {
		t.Fatalf("%v", acks)
	}
	if u := r.srv.Users(); len(u) != 1 || u[0] != "deploy" {
		t.Fatalf("%v", u)
	}
	eventually(t, "audit", func() bool { return len(r.hub.Audit().Recent("", 10)) == 1 })
	e := r.hub.Audit().Recent("web-1", 10)[0]
	if e.Role != "role-a" || e.Project != "my-project" || e.Bot != "bot-1" || e.Result != "ok" || *e.Exit != 3 ||
		e.BytesIn != 5 || e.BytesOut == 0 || strings.Contains(e.Command, "hunter2") || !strings.HasPrefix(e.Command, "cat file") {
		t.Fatalf("%+v", e)
	}
	if r.hub.Streams("") != 0 {
		t.Fatal("stream stays")
	}
}

func TestHostKeyMismatchIsRefusedAndAudited(t *testing.T) {
	r := newRig(t, Limits{})
	line := r.register("web-1", nil, false)
	other, err := sshtest.New() // a different host presents itself at the same address
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	_ = other.Authorize(line)
	r.to.Store(other.Addr())
	_, err = r.open("uptime")
	if err == nil || !strings.Contains(err.Error(), "host key changed") {
		t.Fatalf("%v", err)
	}
	if other.Logins() != 0 || len(other.Commands()) != 0 {
		t.Fatal("authenticated to a server with another host key")
	}
	e := r.hub.Audit().Recent("web-1", 10)
	if len(e) != 1 || e[0].Result != "host key changed" {
		t.Fatalf("%+v", e)
	}
	if v := r.store.List(); v[0].HostKeyChanged == "" {
		t.Fatal("the panel isn't told")
	}
	if r.hub.Streams("") != 0 || r.hub.Streams("role-a") != 0 {
		t.Fatal("slot leaked")
	}
	r.to.Store(r.srv.Addr()) // the right one again clears it
	r.srv.Authorize(line)
	if _, err := r.open("uptime"); err != nil {
		t.Fatal(err)
	}
	if r.store.List()[0].HostKeyChanged != "" {
		t.Fatal("not cleared")
	}
}

func TestOnlyAllowedCommandsRun(t *testing.T) {
	r := newRig(t, Limits{})
	r.register("web-1", []string{"uptime", "tail -n *"}, false)
	if _, err := r.open("uptime; id"); err == nil {
		t.Fatal("ran")
	}
	if _, err := r.open("rm -rf /"); err == nil {
		t.Fatal("ran")
	}
	if len(r.srv.Commands()) != 0 {
		t.Fatalf("%v", r.srv.Commands())
	}
	if _, err := r.open("tail -n 20 /var/log/syslog"); err != nil {
		t.Fatal(err)
	}
	if e := r.hub.Audit().Recent("", 10); len(e) != 2 || e[1].Result != "refused" {
		t.Fatalf("%+v", e)
	}
}

func blockUntilClosed(started chan<- struct{}) sshtest.Handler {
	return func(cmd string, in io.Reader, out, _ io.Writer) int {
		close0(started)
		_, _ = io.Copy(io.Discard, in)
		return 0
	}
}

func close0(c chan<- struct{}) {
	defer func() { _ = recover() }()
	close(c)
}

func TestRevokeCutsALiveStream(t *testing.T) {
	r := newRig(t, Limits{GrantCheck: 20 * time.Millisecond})
	started := make(chan struct{})
	r.srv.Handler = blockUntilClosed(started)
	r.register("web-1", nil, false)
	if _, err := r.open("sleep 100"); err != nil {
		t.Fatal(err)
	}
	<-started
	r.granted.Store(false)
	eventually(t, "cut", func() bool { return len(r.notes.of("ssh.exit")) == 1 })
	if ex := r.notes.of("ssh.exit")[0]; ex["reason"] != "revoked" || ex["code"].(float64) != -1 {
		t.Fatalf("%v", ex)
	}
	eventually(t, "audit", func() bool { return len(r.hub.Audit().Recent("", 5)) == 1 })
	if e := r.hub.Audit().Recent("", 5)[0]; e.Result != "revoked" {
		t.Fatalf("%+v", e)
	}
}

func TestCloseWhereCutsATarget(t *testing.T) {
	r := newRig(t, Limits{})
	started := make(chan struct{})
	r.srv.Handler = blockUntilClosed(started)
	r.register("web-1", nil, false)
	if _, err := r.open("sleep 100"); err != nil {
		t.Fatal(err)
	}
	<-started
	if n := r.hub.CloseWhere(func(_, target string) bool { return target == "other" }, "x"); n != 0 {
		t.Fatal(n)
	}
	if n := r.hub.CloseWhere(func(_, target string) bool { return target == "web-1" }, "target removed"); n != 1 {
		t.Fatal(n)
	}
	eventually(t, "cut", func() bool { return len(r.notes.of("ssh.exit")) == 1 })
}

func TestLimits(t *testing.T) {
	t.Run("streams per role", func(t *testing.T) {
		r := newRig(t, Limits{PerRole: 2})
		started := make(chan struct{})
		r.srv.Handler = blockUntilClosed(started)
		r.register("web-1", nil, false)
		for i := 0; i < 2; i++ {
			if _, err := r.open("sleep 1"); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := r.open("sleep 1"); err == nil || !strings.Contains(err.Error(), "SSH streams open") {
			t.Fatalf("%v", err)
		}
		// Another role has its own.
		if _, err := r.hub.Open(context.Background(), OpenReq{Role: "role-b", Target: "web-1", Command: "x"}, r.notes.add); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("default is four", func(t *testing.T) {
		if (Limits{}).withDefaults().PerRole != 4 || (Limits{}).withDefaults().MaxAge != 2*time.Hour ||
			(Limits{}).withDefaults().Idle != 10*time.Minute || (Limits{}).withDefaults().Buffer != 8<<20 {
			t.Fatal("defaults")
		}
	})
	t.Run("time limit", func(t *testing.T) {
		r := newRig(t, Limits{MaxAge: 100 * time.Millisecond})
		r.srv.Handler = blockUntilClosed(make(chan struct{}))
		r.register("web-1", nil, false)
		r.open("sleep 1")
		eventually(t, "cut", func() bool { return len(r.notes.of("ssh.exit")) == 1 })
		if r.notes.of("ssh.exit")[0]["reason"] != "time limit" {
			t.Fatal(r.notes.all())
		}
	})
	t.Run("idle", func(t *testing.T) {
		r := newRig(t, Limits{Idle: 150 * time.Millisecond})
		r.srv.Handler = blockUntilClosed(make(chan struct{}))
		r.register("web-1", nil, false)
		s, _ := r.open("sleep 1")
		for i := 0; i < 4; i++ { // activity keeps it alive beyond the idle time
			time.Sleep(60 * time.Millisecond)
			s.h.Data("role-a", s.ID(), 0, []byte("x"), false)
		}
		if len(r.notes.of("ssh.exit")) != 0 {
			t.Fatal("cut while active")
		}
		eventually(t, "cut", func() bool { return len(r.notes.of("ssh.exit")) == 1 })
		if r.notes.of("ssh.exit")[0]["reason"] != "idle timeout" {
			t.Fatal(r.notes.all())
		}
	})
	t.Run("input buffer", func(t *testing.T) {
		r := newRig(t, Limits{Buffer: 1024})
		r.srv.Handler = blockUntilClosed(make(chan struct{}))
		r.register("web-1", nil, false)
		s, _ := r.open("cat")
		s.h.Data("role-a", s.ID(), 0, make([]byte, 4096), false) // more than the window the runner may fill
		eventually(t, "cut", func() bool { return len(r.notes.of("ssh.exit")) == 1 })
		if r.notes.of("ssh.exit")[0]["reason"] != "input buffer overrun" {
			t.Fatal(r.notes.all())
		}
	})
	t.Run("output backpressure", func(t *testing.T) {
		r := newRig(t, Limits{Buffer: 4096})
		var sent atomic.Int64
		r.srv.Handler = func(cmd string, in io.Reader, out, _ io.Writer) int {
			blk := make([]byte, 1024)
			for i := 0; i < 64; i++ { // 64 KiB
				if _, err := out.Write(blk); err != nil {
					return 1
				}
				sent.Add(1024)
			}
			return 0
		}
		r.register("web-1", nil, false)
		s, _ := r.open("yes")
		time.Sleep(300 * time.Millisecond) // nothing is acknowledged: Home stops reading at the window
		var got int
		for _, p := range r.notes.of("ssh.data") {
			b, _ := base64.StdEncoding.DecodeString(p["b64"].(string))
			got += len(b)
		}
		if got != 4096 || len(r.notes.of("ssh.exit")) != 0 {
			t.Fatalf("relayed %d bytes ahead of acknowledgements", got)
		}
		acked := 0
		eventually(t, "exit", func() bool { // the runner consumes what arrived: the rest flows
			n := len(r.notes.output(1))
			if n > acked {
				s.h.Ack("role-a", s.ID(), int64(n-acked))
				acked = n
			}
			return len(r.notes.of("ssh.exit")) == 1
		})
		if got := len(r.notes.output(1)); got != 64*1024 {
			t.Fatalf("%d", got)
		}
	})
}

func TestAStreamBelongsToItsRole(t *testing.T) {
	r := newRig(t, Limits{})
	started := make(chan struct{})
	r.srv.Handler = blockUntilClosed(started)
	r.register("web-1", nil, false)
	s, _ := r.open("sleep 1")
	<-started
	r.hub.Close("role-b", s.ID()) // another role's runner can't touch it
	time.Sleep(100 * time.Millisecond)
	if len(r.notes.of("ssh.exit")) != 0 {
		t.Fatal("closed by another role")
	}
	r.hub.Close("role-a", s.ID())
	eventually(t, "cut", func() bool { return len(r.notes.of("ssh.exit")) == 1 })
}

func TestPrivateKeyNeverLeaves(t *testing.T) {
	r := newRig(t, Limits{})
	line := r.register("web-1", nil, false)
	pemb, err := os.ReadFile(filepath.Join(r.dir, "keys", "web-1"))
	if err != nil {
		t.Fatal(err)
	}
	sg, err := ssh.ParseRawPrivateKey(pemb)
	if err != nil {
		t.Fatal(err)
	}
	pk, ok := sg.(*ed25519.PrivateKey)
	if !ok {
		t.Fatalf("%T", sg)
	}
	seed := pk.Seed()
	needles := [][]byte{seed, []byte(base64.StdEncoding.EncodeToString(seed)), []byte(base64.RawStdEncoding.EncodeToString(seed))}
	for _, l := range strings.Split(strings.TrimSpace(string(pemb)), "\n")[1:] {
		if !strings.HasPrefix(l, "-----") && len(l) > 30 {
			needles = append(needles, []byte(l))
		}
	}
	r.srv.Handler = func(cmd string, in io.Reader, out, _ io.Writer) int { io.WriteString(out, "ok"); return 0 }
	_, _ = r.open("uptime")
	eventually(t, "exit", func() bool { return len(r.notes.of("ssh.exit")) == 1 })
	_, _ = r.open("rm")
	var all [][]byte
	all = append(all, []byte(line), []byte(strings.Join(r.notes.all(), "\n")))
	for _, v := range r.store.List() {
		b, _ := json.Marshal(v)
		all = append(all, b)
	}
	a, _ := json.Marshal(r.hub.Audit().Recent("", 100))
	all = append(all, a)
	raw, _ := os.ReadFile(filepath.Join(r.dir, "audit.jsonl"))
	all = append(all, raw)
	for _, hay := range all {
		for _, n := range needles {
			if bytes.Contains(hay, n) {
				t.Fatalf("key material in %q", hay[:min(len(hay), 80)])
			}
		}
	}
}

func TestRemovedNameNeedsReuse(t *testing.T) {
	r := newRig(t, Limits{})
	r.register("web-1", nil, false)
	if err := r.store.Remove("web-1"); err != nil {
		t.Fatal(err)
	}
	args := RegisterArgs{Name: "web-1", Host: "web-1.example.com", User: "deploy", Fingerprint: r.srv.Fingerprint()}
	if _, err := r.store.Register(context.Background(), args); err == nil {
		t.Fatal("a removed name was registered again without reuse")
	}
	// The tombstone survives a restart.
	s2, err := Open(r.dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.removed["web-1"] {
		t.Fatal("tombstone lost on restart")
	}
	args.Reuse = true
	if _, err := r.store.Register(context.Background(), args); err != nil {
		t.Fatalf("reuse refused: %v", err)
	}
	if r.store.removed["web-1"] {
		t.Fatal("tombstone not cleared by reuse")
	}
}
