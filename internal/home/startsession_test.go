package home

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLauncher records launches and never starts anything. onLaunch runs inside Launch (it can play hooks).
type fakeLauncher struct {
	mu       sync.Mutex
	cwds     []string
	prompts  []string
	err      error
	onLaunch func(cwd string)
}

func (f *fakeLauncher) Launch(_ context.Context, cwd, prompt string) error {
	f.mu.Lock()
	f.cwds = append(f.cwds, cwd)
	f.prompts = append(f.prompts, prompt)
	err, on := f.err, f.onLaunch
	f.mu.Unlock()
	if on != nil {
		on(cwd)
	}
	return err
}

func (f *fakeLauncher) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.cwds) }

type startEnv struct {
	c     *Claude
	l     *fakeLauncher
	home  string // a fake home directory
	work  string // a directory that is not seen and not allow-listed
	dirs  []string
	mkdir func(parts ...string) string
}

func newStartEnv(t *testing.T) *startEnv {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &startEnv{l: &fakeLauncher{}, home: filepath.Join(base, "home"), work: filepath.Join(base, "work")}
	e.mkdir = func(parts ...string) string {
		p := filepath.Join(parts...)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	e.mkdir(e.home)
	e.mkdir(e.work)
	e.c = &Claude{Transcripts: t.TempDir(), Launcher: e.l, HomeDir: e.home,
		StartDirs: func() []string { return e.dirs }, systemDirs: []string{"/etc", "/usr"}} // temp dirs may sit under /var
	old := [4]time.Duration{claudeStartWait, claudeStartPoll, claudeLaunchTimeout, claudeNonceGrace}
	claudeStartWait, claudeStartPoll, claudeNonceGrace = 300*time.Millisecond, 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() {
		claudeStartWait, claudeStartPoll, claudeLaunchTimeout, claudeNonceGrace = old[0], old[1], old[2], old[3]
	})
	return e
}

func (e *startEnv) sessionStart(id, cwd string) {
	e.c.Hook([]byte(`{"hook_event_name":"SessionStart","session_id":"` + id + `","cwd":"` + cwd + `","permission_mode":"bypassPermissions","source":"startup"}`))
}

func TestStartDirValidation(t *testing.T) {
	e := newStartEnv(t)
	ctx := context.Background()
	seen := e.mkdir(e.work, "seen")
	e.sessionStart("old", seen)
	allowed := e.mkdir(e.work, "allowed")
	sub := e.mkdir(allowed, "sub", "deep")
	sibling := e.mkdir(e.work, "allowed2")
	e.dirs = []string{allowed}
	outside := e.mkdir(e.work, "outside")
	if err := os.Symlink(outside, filepath.Join(allowed, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.home, ".ssh"), filepath.Join(allowed, "tossh")); err != nil {
		t.Fatal(err)
	}
	e.mkdir(e.home, ".ssh")
	e.mkdir(e.home, ".claude", "projects")
	e.mkdir(e.home, "Library", "Application Support")
	project := e.mkdir(e.home, "code", "proj")
	file := filepath.Join(e.work, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// a symlink to a seen directory is the same directory
	if err := os.Symlink(seen, filepath.Join(e.work, "seenlink")); err != nil {
		t.Fatal(err)
	}
	helperDir := e.mkdir(e.work, "helper-dir")
	e.c.ProtectedDirs = []string{helperDir}

	ok := []string{seen, allowed, sub}
	for _, d := range ok {
		if _, err := e.c.checkStartDir(ctx, d); err != nil {
			t.Errorf("%s: %v", d, err)
		}
	}
	bad := map[string]string{
		"relative":          "proj",
		"empty":             "",
		"unclean":           allowed + "/../allowed",
		"trailing slash":    allowed + "/",
		"control":           allowed + "\n",
		"unknown":           e.work,
		"sibling by prefix": sibling,
		"missing":           filepath.Join(allowed, "nope"),
		"a file":            file,
		"escape by link":    filepath.Join(allowed, "link"),
		"link to ~/.ssh":    filepath.Join(allowed, "tossh"),
		"ssh":               filepath.Join(e.home, ".ssh"),
		"claude":            filepath.Join(e.home, ".claude", "projects"),
		"library":           filepath.Join(e.home, "Library", "Application Support"),
		"home":              e.home,
		"root":              "/",
		"etc":               "/etc",
		"helper dir":        helperDir,
		"unseen project":    project,
		"symlink to seen":   filepath.Join(e.work, "seenlink"), // the approved path must be the real one
		".local":            e.mkdir(e.home, ".local", "share"),
		".npm":              e.mkdir(e.home, ".npm", "x"),
		".zsh_sessions":     e.mkdir(e.home, ".zsh_sessions"),
		".oh-my-zsh":        e.mkdir(e.home, ".oh-my-zsh", "custom"),
	}
	for name, d := range bad {
		if r, err := e.c.checkStartDir(ctx, d); err == nil {
			t.Errorf("%s (%q) was allowed as %q", name, d, r)
		}
	}
	// A seen directory under a sensitive one is still refused, and so is an allow-list entry for it.
	e.sessionStart("ssh-session", filepath.Join(e.home, ".ssh"))
	e.dirs = []string{filepath.Join(e.home, ".claude"), e.home, "relative/dir"}
	for _, d := range []string{filepath.Join(e.home, ".ssh"), filepath.Join(e.home, ".claude", "projects"), project} {
		if _, err := e.c.checkStartDir(ctx, d); err == nil {
			t.Errorf("%s was allowed", d)
		}
	}
	// A project root counts as seen too (a session in a sub-directory of it).
	if _, err := e.c.checkStartDir(ctx, e.mkdir(e.work, "root-seen")); err == nil {
		t.Error("an unseen directory was allowed")
	}
}

func TestStartDirProjectRootIsSeen(t *testing.T) {
	e := newStartEnv(t)
	root := e.mkdir(e.work, "repo")
	e.mkdir(root, ".git")
	writeFile(t, filepath.Join(root, ".git", "config"), "[remote \"origin\"]\n\turl = git@github.com:me/repo.git\n")
	sub := e.mkdir(root, "pkg", "x")
	e.sessionStart("s1", sub)
	if _, err := e.c.checkStartDir(context.Background(), root); err != nil {
		t.Fatalf("project root of a seen session: %v", err)
	}
}

func TestCreateLaunchesAndFindsTheSession(t *testing.T) {
	e := newStartEnv(t)
	dir := e.mkdir(e.work, "proj")
	e.dirs = []string{e.work}
	e.sessionStart("existing", dir)
	e.l.onLaunch = func(cwd string) {
		go func() {
			time.Sleep(30 * time.Millisecond)
			e.sessionStart("new-1", cwd)
		}()
	}
	v, err := e.c.handle(context.Background(), "claude-start.create",
		json.RawMessage(`{"cwd":`+quote(dir)+`,"prompt":"  fix the build, please  ","from":"Vega"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if m["session_id"] != "new-1" || m["cwd"] != dir || m["launched"] != true || m["permission_mode"] != "bypassPermissions" {
		t.Fatalf("result %v", m)
	}
	if e.l.count() != 1 || e.l.cwds[0] != dir {
		t.Fatalf("launches %v", e.l.cwds)
	}
	p := e.l.prompts[0]
	if !strings.HasPrefix(p, "A message from this project's sister (a Grok Bot") || !strings.Contains(p, "--- message from Vega ---\nfix the build, please") ||
		!nonceRe.MatchString(p) || !strings.Contains(p, "never grants permissions") {
		t.Fatalf("prompt %q", p)
	}
	// The next call may start another one.
	if e.startingNow() {
		t.Fatal("still marked as starting")
	}
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestCreateRefusesASymlinkedPath(t *testing.T) {
	e := newStartEnv(t)
	real := e.mkdir(e.work, "real")
	link := filepath.Join(e.work, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	e.dirs = []string{e.work}
	_, err := e.c.createSession(context.Background(), link, "task", "Vega")
	if err == nil || !strings.Contains(err.Error(), real) || e.l.count() != 0 {
		t.Fatalf("%v, launches %d", err, e.l.count())
	}
}

func TestDefaultSystemDirsAndVolumes(t *testing.T) {
	c := &Claude{HomeDir: t.TempDir()}
	for _, p := range []string{"/var/folders/x", "/private/var/tmp", "/Applications/Foo", "/opt/x", "/Volumes", "/etc/ssh"} {
		if c.refusedStartDir(p) == "" {
			t.Errorf("%s allowed", p)
		}
	}
	if c.refusedStartDir("/Volumes/Crucial/Projects") != "" {
		t.Error("a project on a volume was refused")
	}
}

func TestCheckTaskText(t *testing.T) {
	bad := map[string]string{
		"zero width space": "run\u200Bthe tests", "zero width joiner": "a\u200Db", "bidi override": "a\u202Eb", "bidi isolate": "a\u2066b",
		"bom": "\ufeffa", "tag char": "a\U000E0041b", "private use": "a\ue000b", "C1 control": "a\u0085b", "carriage return": "a\r\nb",
		"tab": "a\tb", "escape": "a\x1b[31mb", "line separator": "a\u2028b", "null": "a\x00b", "invalid utf8": "a\xffb",
		"blank run": "a\n\n\n\nb", "blank run with spaces": "a\n \n  \n\t\nb", "too many lines": strings.Repeat("x\n", claudeMaxTaskLines),
	}
	for name, s := range bad {
		if checkTaskText(s) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for name, s := range map[string]string{"plain": "run the tests", "unicode": "日本語 ünï é emoji 🙂 – “quotes”", "two blank lines": "a\n\n\nb",
		"lines": strings.Repeat("x\n", claudeMaxTaskLines-1) + "x", "code": "if a && b {\n    c()\n}"} {
		if err := checkTaskText(s); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// What is refused is never changed and launched: create rejects it before any launch.
	e := newStartEnv(t)
	dir := e.mkdir(e.work, "proj")
	e.dirs = []string{e.work}
	if _, err := e.c.createSession(context.Background(), dir, "run\u200Bthe tests", "V"); err == nil || e.l.count() != 0 {
		t.Fatalf("hidden text launched: %v", err)
	}
}

var nonceRe = regexp.MustCompile(`rubi-[0-9a-f]{12}`)

func (e *startEnv) transcript(t *testing.T, name, firstUser string) string {
	t.Helper()
	p := filepath.Join(e.c.Transcripts, name+".jsonl")
	if firstUser != "" {
		m, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": firstUser}})
		writeFile(t, p, `{"type":"summary"}`+"\n"+string(m)+"\n")
	}
	return p
}

func (e *startEnv) hookStart(id, cwd, source, transcript string) {
	b, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": id, "cwd": cwd, "source": source, "transcript_path": transcript})
	e.c.Hook(b)
}

func TestCreateNeedsAStartupAndMatchesTheReferenceCode(t *testing.T) {
	e := newStartEnv(t)
	dir := e.mkdir(e.work, "proj")
	e.dirs = []string{e.work}
	// A resumed or cleared session in the same directory is not a new one.
	e.l.onLaunch = func(cwd string) {
		e.hookStart("resumed", cwd, "resume", "")
		e.hookStart("cleared", cwd, "clear", "")
	}
	v, err := e.c.createSession(context.Background(), dir, "task", "V")
	if err != nil || v.(map[string]any)["session_id"] != "" {
		t.Fatalf("resume/clear counted: %v %v", v, err)
	}
	e.c.startMu.Lock()
	e.c.startHits = nil
	e.c.startMu.Unlock()
	// Another session starts in the same directory (its first message has another code), then ours.
	e.l.onLaunch = func(cwd string) {
		code := nonceRe.FindString(e.l.prompts[len(e.l.prompts)-1])
		e.hookStart("stranger", cwd, "startup", e.transcript(t, "stranger", "hello, no code here"))
		e.hookStart("ours", cwd, "startup", e.transcript(t, "ours", "A message ... reference: "+code+" ... --- message from V ---\ntask"))
	}
	v, err = e.c.createSession(context.Background(), dir, "task", "V")
	if err != nil || v.(map[string]any)["session_id"] != "ours" {
		t.Fatalf("with the code: %v %v", v, err)
	}
	if !nonceRe.MatchString(e.l.prompts[len(e.l.prompts)-1]) {
		t.Fatal("no reference code in the prompt")
	}
	// A transcript that isn't readable yet: the hook alone is enough after a moment.
	e.c.startMu.Lock()
	e.c.startHits = nil
	e.c.startMu.Unlock()
	e.l.onLaunch = func(cwd string) { e.hookStart("early", cwd, "startup", e.transcript(t, "early", "")) }
	v, err = e.c.createSession(context.Background(), dir, "task", "V")
	if err != nil || v.(map[string]any)["session_id"] != "early" {
		t.Fatalf("unreadable transcript: %v %v", v, err)
	}
	// Two sessions with the code's absence... both unreadable: ambiguous, no id.
	e.c.startMu.Lock()
	e.c.startHits = nil
	e.c.startMu.Unlock()
	e.l.onLaunch = func(cwd string) {
		e.hookStart("a", cwd, "startup", "")
		e.hookStart("b", cwd, "startup", "")
	}
	v, err = e.c.createSession(context.Background(), dir, "task", "V")
	if err != nil || v.(map[string]any)["session_id"] != "" {
		t.Fatalf("ambiguous: %v %v", v, err)
	}
}

func TestTranscriptOutsideTheProjectsDirIsNotRead(t *testing.T) {
	e := newStartEnv(t)
	other := filepath.Join(e.work, "t.jsonl")
	writeFile(t, other, `{"type":"user","message":"nonce-x"}`+"\n")
	if e.c.transcriptNonce(other, "nonce-x") != nonceUnknown {
		t.Fatal("read a file outside the projects directory")
	}
}

func TestCreateTimeoutAndIgnoredHooks(t *testing.T) {
	e := newStartEnv(t)
	dir := e.mkdir(e.work, "proj")
	other := e.mkdir(e.work, "other")
	e.dirs = []string{e.work}
	e.sessionStart("known", dir)
	e.l.onLaunch = func(cwd string) {
		e.sessionStart("known", cwd)                                                                                // an old session starting again (resume, /clear)
		e.sessionStart("elsewhere", other)                                                                          // another directory
		e.c.Hook([]byte(`{"hook_event_name":"UserPromptSubmit","session_id":"never-started","cwd":"` + cwd + `"}`)) // not a SessionStart
	}
	v, err := e.c.createSession(context.Background(), dir, "task", "Vega")
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if m["launched"] != true || m["session_id"] != "" || !strings.Contains(m["note"].(string), "no new session") {
		t.Fatalf("timeout result %v", m)
	}
}

func TestCreateAmbiguousIsNotGuessed(t *testing.T) {
	e := newStartEnv(t)
	dir := e.mkdir(e.work, "proj")
	e.dirs = []string{e.work}
	e.l.onLaunch = func(cwd string) { e.sessionStart("a", cwd); e.sessionStart("b", cwd) }
	v, err := e.c.createSession(context.Background(), dir, "task", "")
	if err != nil {
		t.Fatal(err)
	}
	if m := v.(map[string]any); m["session_id"] != "" || !strings.Contains(m["note"].(string), "several") {
		t.Fatalf("%v", m)
	}
	if !strings.Contains(e.l.prompts[0], "--- message from the sister ---") {
		t.Fatalf("no sender name: %q", e.l.prompts[0])
	}
}

func TestCreateRateLimit(t *testing.T) {
	e := newStartEnv(t)
	dir := e.mkdir(e.work, "proj")
	e.dirs = []string{e.work}
	claudeStartWait = 20 * time.Millisecond
	for i := 0; i < claudeStartsPerHour; i++ {
		if _, err := e.c.createSession(context.Background(), dir, "task", "V"); err != nil {
			t.Fatalf("launch %d: %v", i, err)
		}
	}
	// Refused requests (bad directory, bad prompt) neither count nor launch.
	if _, err := e.c.createSession(context.Background(), e.home, "task", "V"); err == nil {
		t.Fatal("home allowed")
	}
	_, err := e.c.createSession(context.Background(), dir, "task", "V")
	if err == nil || !strings.Contains(err.Error(), "per hour") || e.l.count() != claudeStartsPerHour {
		t.Fatalf("4th: %v, launches %d", err, e.l.count())
	}
	// An hour later the oldest hits are forgotten.
	e.c.startMu.Lock()
	for i := range e.c.startHits {
		e.c.startHits[i] = e.c.startHits[i].Add(-2 * time.Hour)
	}
	e.c.startMu.Unlock()
	if _, err := e.c.createSession(context.Background(), dir, "task", "V"); err != nil {
		t.Fatalf("after an hour: %v", err)
	}
}

func TestCreatePromptLimits(t *testing.T) {
	e := newStartEnv(t)
	dir := e.mkdir(e.work, "proj")
	e.dirs = []string{e.work}
	claudeStartWait = 10 * time.Millisecond
	for _, p := range []string{"", "   \n", "\a\a"} {
		if _, err := e.c.createSession(context.Background(), dir, p, "V"); err == nil {
			t.Errorf("%q was accepted", p)
		}
	}
	if _, err := e.c.createSession(context.Background(), dir, strings.Repeat("x", claudeMaxTask+1), "V"); err == nil {
		t.Error("2001 characters accepted")
	}
	if _, err := e.c.createSession(context.Background(), dir, strings.Repeat("é", claudeMaxTask), "V"); err != nil {
		t.Errorf("2000 characters refused: %v", err)
	}
	if e.l.count() != 1 {
		t.Fatalf("launches %d", e.l.count())
	}
}

func TestCreateOneAtATime(t *testing.T) {
	e := newStartEnv(t)
	dir := e.mkdir(e.work, "proj")
	e.dirs = []string{e.work}
	claudeStartWait = 400 * time.Millisecond
	entered := make(chan struct{})
	e.l.onLaunch = func(string) { close(entered) }
	done := make(chan error, 1)
	go func() { _, err := e.c.createSession(context.Background(), dir, "task", "V"); done <- err }()
	<-entered
	if _, err := e.c.createSession(context.Background(), dir, "other task", "V"); err == nil || !strings.Contains(err.Error(), "already being started") {
		t.Fatalf("second: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if e.l.count() != 1 {
		t.Fatalf("launches %d", e.l.count())
	}
}

func TestCreateLauncherErrorAndMissingLauncher(t *testing.T) {
	e := newStartEnv(t)
	dir := e.mkdir(e.work, "proj")
	e.dirs = []string{e.work}
	e.l.err = os.ErrPermission
	if _, err := e.c.createSession(context.Background(), dir, "task", "V"); err == nil {
		t.Fatal("launcher error swallowed")
	}
	if e.startingNow() {
		t.Fatal("still marked as starting after an error")
	}
	e.c.Launcher = nil
	if _, err := e.c.createSession(context.Background(), dir, "task", "V"); err == nil || !strings.Contains(err.Error(), "can't start") {
		t.Fatalf("no launcher: %v", err)
	}
	if _, err := e.c.handle(context.Background(), "claude-start.create", json.RawMessage(`[1]`)); err == nil {
		t.Fatal("bad arguments accepted")
	}
}

func TestCreateStopsWithTheRequest(t *testing.T) {
	e := newStartEnv(t)
	dir := e.mkdir(e.work, "proj")
	e.dirs = []string{e.work}
	claudeStartWait = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	if _, err := e.c.createSession(ctx, dir, "task", "V"); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
	deadline := time.Now().Add(2 * time.Second)
	for e.startingNow() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.startingNow() {
		t.Fatal("the flight never ended")
	}
}

func (e *startEnv) startingNow() bool {
	e.c.startMu.Lock()
	defer e.c.startMu.Unlock()
	return e.c.starting
}

func TestDesktopURLEncoding(t *testing.T) {
	cwd := "/Users/a b/ü & #proj+1/%x"
	prompt := "fix a&b=c #1 + 100% ünï 日本語\nline two?"
	u := desktopURL(cwd, prompt)
	if !strings.HasPrefix(u, "claude://code/new?folder=") || strings.ContainsAny(u, " +#\n") || strings.Count(u, "&") != 1 || strings.Count(u, "?") != 1 {
		t.Fatalf("url %q", u)
	}
	p, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	q := p.Query()
	if p.Scheme != "claude" || p.Host != "code" || p.Path != "/new" || q.Get("folder") != cwd || q.Get("q") != prompt {
		t.Fatalf("round trip: %q %q (%s)", q.Get("folder"), q.Get("q"), u)
	}
}

func TestDesktopLauncherOpensTheURL(t *testing.T) {
	var name string
	var args []string
	l := desktopLauncher{goos: "darwin", run: func(_ context.Context, n string, a ...string) error { name, args = n, a; return nil }}
	if err := l.Launch(context.Background(), "/p", "hi there"); err != nil {
		t.Fatal(err)
	}
	if name != "/usr/bin/open" || len(args) != 1 || args[0] != "claude://code/new?folder=%2Fp&q=hi%20there" {
		t.Fatalf("%s %q", name, args)
	}
	l.run = func(context.Context, string, ...string) error { return os.ErrNotExist }
	if err := l.Launch(context.Background(), "/p", "x"); err == nil || !strings.Contains(err.Error(), "installed") {
		t.Fatalf("open failure: %v", err)
	}
	called := false
	l = desktopLauncher{goos: "linux", run: func(context.Context, string, ...string) error { called = true; return nil }}
	if err := l.Launch(context.Background(), "/p", "x"); err == nil || called {
		t.Fatalf("linux: %v called=%v", err, called)
	}
}

func TestOpenWiresTheLauncherAndStartDirs(t *testing.T) {
	dir := t.TempDir()
	h, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if h.Claude.Launcher == nil || len(h.Claude.StartDirs()) != 0 || len(h.Claude.ProtectedDirs) != 1 {
		t.Fatalf("launcher %v dirs %v", h.Claude.Launcher, h.Claude.StartDirs())
	}
	if err := h.Update(func(c *Config) { c.ClaudeStartDirs = []string{"/a/b"} }); err != nil {
		t.Fatal(err)
	}
	if got := h.Claude.StartDirs(); len(got) != 1 || got[0] != "/a/b" {
		t.Fatalf("%v", got)
	}
}
