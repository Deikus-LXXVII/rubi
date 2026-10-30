package home

// The registry of Claude Code sessions on this computer (claude-sessions.list). It reads, never writes:
//   - the Claude desktop app's session store (<user config dir>/Claude/claude-code-sessions/<account>/<org>/
//     local_<uuid>.json): title, working directory, the session it was spawned from;
//   - Claude Code's transcripts (~/.claude/projects/<slug>/<session id>.jsonl), for sessions started
//     from the command line;
//   - what the hooks report (SessionStart, UserPromptSubmit, Stop, SessionEnd): which sessions are live
//     and whether they are in a turn.
// The project of a session is its git remote (read from .git/config, no git is run), or its top
// directory when it has none, so worktrees of one repository belong together.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sessionScanEvery   = 3 * time.Second
	maxStoreFiles      = 2000
	maxStoreFile       = 1 << 20
	maxTranscriptDirs  = 300
	maxTranscripts     = 200
	transcriptMaxAge   = 30 * 24 * time.Hour
	transcriptHeadRead = 64 << 10
	maxSessionsListed  = 200

	// A project directory can sit on a disk that stops answering (an external drive that hangs, a
	// network volume): a read there blocks in the kernel and can't be cancelled. The scan waits at most
	// gitScanBudget in total for git info, then lists the remaining sessions without it while the reads
	// finish in the background (at most maxGitReads of them at a time).
	gitScanBudget = 2 * time.Second
	maxGitReads   = 16
)

// The scan of the stores runs in the background, off every lock the hooks use. The same kind of read can
// hang there, so the scan stops reading more files after sessionScanBudget; a single read that never
// returns holds only the one scan goroutine, and lists go on serving the last result. Before the first
// scan has finished, a list waits for it at most scanWait (the registry's wait when set). hookGitWait is how long a list waits for git
// info of hook-reported sessions that no scan has seen.
const (
	scanWait = 6 * time.Second
	// A scan whose read never returns would keep every later scan out: after 3 scan budgets a list may
	// start one replacement (the stuck one's late result is dropped), and at most maxOldScans stuck scans
	// are left behind in total.
	scanStuckFactor = 3
	maxOldScans     = 3
)

var (
	sessionScanBudget = 4 * time.Second
	hookGitWait       = 200 * time.Millisecond
)

// ClaudeSession is one Claude Code session as claude-sessions.list reports it. Title comes from the user or
// Claude and is untrusted text.
type ClaudeSession struct {
	SessionID      string    `json:"session_id"`               // Claude Code's id (what hooks carry)
	AppSessionID   string    `json:"app_session_id,omitempty"` // the desktop app's id (local_…), stable across restarts
	Title          string    `json:"title,omitempty"`
	Cwd            string    `json:"cwd,omitempty"`
	ProjectRoot    string    `json:"project_root,omitempty"`
	RepoKey        string    `json:"repo_key,omitempty"`
	Branch         string    `json:"branch,omitempty"`
	Role           string    `json:"role"`             // "main", or "sub" (spawned from another session or in a .claude/worktrees worktree)
	Parent         string    `json:"parent,omitempty"` // the app session it was spawned from
	Source         string    `json:"source"`           // "desktop", "transcript" or "hook"
	PermissionMode string    `json:"permission_mode,omitempty"`
	Archived       bool      `json:"archived,omitempty"`
	State          string    `json:"state"` // "busy" (in a turn), "idle", "ended" or "unknown" (no hook seen)
	LastActivity   time.Time `json:"last_activity,omitzero"`
	Waiting        int       `json:"waiting,omitempty"` // messages in its inbox
}

type liveSession struct {
	cwd, transcript, mode, state string
	last                         time.Time
}

// The registry has two locks, never held together. liveMu guards only live (what the hooks reported): touch
// and transcriptOf take it for a few map operations, so a hook never waits for a scan. mu guards the scan
// cache, the git cache and the scan state; nothing that can block (a file read, a wait) runs under it.
type sessionRegistry struct {
	liveMu sync.Mutex
	live   map[string]*liveSession

	mu        sync.Mutex
	scanned   []ClaudeSession
	scannedAt time.Time
	hasScan   bool          // scanned holds a finished scan (it may be empty)
	scanning  bool          // the current scan is running
	scanDone  chan struct{} // closed when the current scan ends
	scanGen   uint64        // the current scan's number; a result from an older scan is dropped
	scanStart time.Time     // when the current scan started
	oldScans  int           // replaced scans that haven't returned yet
	git       map[string]gitInfo
	reading   map[string]*gitRead // directories whose git info is being read in the background
	// readGit reads git info (readGitInfo when nil; a seam for tests). readStore and readTx read the
	// desktop store and the transcripts (readDesktopStore and readTranscripts when nil; seams too).
	readGit   func(dir string) gitInfo
	readStore func(dir string, deadline time.Time) []ClaudeSession
	readTx    func(dir string, now, deadline time.Time) []ClaudeSession
	// firstScanWait replaces scanWait, and scanStuckAfter 3*sessionScanBudget, when set (seams for tests).
	firstScanWait, scanStuckAfter time.Duration
}

// gitRead is a git info read in progress; g is set before done is closed.
type gitRead struct {
	done chan struct{}
	g    gitInfo
}

// touch records what a hook said about a session.
func (r *sessionRegistry) touch(id, cwd, transcript, mode, state string, now time.Time) {
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	if r.live == nil {
		r.live = map[string]*liveSession{}
	}
	s := r.live[id]
	if s == nil {
		if len(r.live) >= 500 { // forget the oldest
			old, oldAt := "", now
			for k, v := range r.live {
				if v.last.Before(oldAt) {
					old, oldAt = k, v.last
				}
			}
			delete(r.live, old)
		}
		s = &liveSession{}
		r.live[id] = s
	}
	if cwd != "" {
		s.cwd = cwd
	}
	if transcript != "" {
		s.transcript = transcript
	}
	if mode != "" {
		s.mode = mode
	}
	if state != "" {
		s.state = state
	}
	s.last = now
}

func (r *sessionRegistry) transcriptOf(id string) string {
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	if s := r.live[id]; s != nil {
		return s.transcript
	}
	return ""
}

// list merges the stores and the hooks. desktop and transcripts are directories ("" to skip). It answers
// from the last scan, however old, while a new one runs in the background; only before the first scan
// finished does it wait for it, for at most scanWait, and then lists the hook-reported sessions alone.
func (r *sessionRegistry) list(desktop, transcripts string, now time.Time) []ClaudeSession {
	scanned := r.scanResult(desktop, transcripts, now)
	r.liveMu.Lock()
	live := make(map[string]liveSession, len(r.live))
	for id, l := range r.live {
		live[id] = *l
	}
	r.liveMu.Unlock()

	out := make([]ClaudeSession, 0, len(scanned)+len(live))
	seen := map[string]bool{}
	for _, s := range scanned {
		if l, ok := live[s.SessionID]; ok && s.SessionID != "" {
			s.State = l.state
			if l.last.After(s.LastActivity) {
				s.LastActivity = l.last
			}
			if l.mode != "" {
				s.PermissionMode = l.mode
			}
			if l.cwd != "" && s.Cwd == "" {
				s.Cwd = l.cwd
			}
		}
		if s.SessionID != "" {
			seen[s.SessionID] = true
		}
		out = append(out, s)
	}
	gitBy := time.Now().Add(hookGitWait)
	for id, l := range live {
		if seen[id] {
			continue
		}
		s := ClaudeSession{SessionID: id, Cwd: l.cwd, Source: "hook", State: l.state, PermissionMode: l.mode, LastActivity: l.last}
		r.fillProject(&s, l.cwd, gitBy)
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastActivity.After(out[j].LastActivity) })
	return out
}

// scanResult is the last finished scan, starting a new one when it is older than sessionScanEvery.
func (r *sessionRegistry) scanResult(desktop, transcripts string, now time.Time) []ClaudeSession {
	r.mu.Lock()
	if r.hasScan && now.Sub(r.scannedAt) <= sessionScanEvery {
		out := r.scanned
		r.mu.Unlock()
		return out
	}
	stuckAfter := r.scanStuckAfter
	if stuckAfter <= 0 {
		stuckAfter = scanStuckFactor * sessionScanBudget
	}
	replace := r.scanning && r.oldScans < maxOldScans && time.Since(r.scanStart) > stuckAfter
	if !r.scanning || replace { // single-flight: one current scan at a time, whoever asks
		if replace {
			log.Printf("claude-sessions: a scan of the session stores has hung; starting another")
			r.oldScans++
		}
		r.scanning = true
		r.scanGen++
		r.scanStart = time.Now()
		r.scanDone = make(chan struct{})
		go r.scan(desktop, transcripts, now, r.scanGen, r.scanDone)
	}
	done, have, out := r.scanDone, r.hasScan, r.scanned
	r.mu.Unlock()
	if have {
		return out
	}
	wait := scanWait
	if r.firstScanWait > 0 {
		wait = r.firstScanWait
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		log.Printf("claude-sessions: the first scan of the session stores isn't done (is a disk responding?); listing hook-reported sessions only")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.scanned
}

// scan reads the stores and git info with no lock held and then stores the result, unless a newer scan
// has replaced this one meanwhile. It ends the scan (and releases waiters) even when a read panics.
func (r *sessionRegistry) scan(desktop, transcripts string, now time.Time, gen uint64, done chan struct{}) {
	var out []ClaudeSession
	ok := false
	defer func() {
		if p := recover(); p != nil {
			log.Printf("claude-sessions: scanning the session stores panicked: %v", p)
		}
		r.mu.Lock()
		switch {
		case gen != r.scanGen: // replaced while stuck: its result is stale
			r.oldScans--
		case ok:
			r.scanned, r.scannedAt, r.hasScan = out, now, true
			r.scanning = false
		default: // keep the old result; with none, an empty one until the next scan
			if !r.hasScan {
				r.scanned, r.scannedAt, r.hasScan = []ClaudeSession{}, now, true
			}
			r.scanning = false
		}
		r.mu.Unlock()
		close(done)
	}()
	out = r.scanFiles(desktop, transcripts, now)
	ok = true
}

func (r *sessionRegistry) scanFiles(desktop, transcripts string, now time.Time) []ClaudeSession {
	out := []ClaudeSession{}
	seen := map[string]bool{}
	start := time.Now()
	readBy, gitBy := start.Add(sessionScanBudget), start.Add(gitScanBudget)
	readStore, readTx := r.readStore, r.readTx
	if readStore == nil {
		readStore = readDesktopStore
	}
	if readTx == nil {
		readTx = readTranscripts
	}
	for _, s := range readStore(desktop, readBy) {
		r.fillProject(&s, s.ProjectRoot, gitBy)
		if s.SessionID != "" {
			seen[s.SessionID] = true
		}
		out = append(out, s)
	}
	for _, s := range readTx(transcripts, now, readBy) {
		if seen[s.SessionID] {
			continue
		}
		r.fillProject(&s, s.Cwd, gitBy)
		out = append(out, s)
	}
	return out
}

// fillProject sets the project from dir (the origin directory for desktop sessions, otherwise the
// working directory) and the role from the working directory. Git info is waited for until gitBy.
func (r *sessionRegistry) fillProject(s *ClaudeSession, dir string, gitBy time.Time) {
	if dir == "" {
		dir = s.Cwd
	}
	if s.State == "" {
		s.State = "unknown"
	}
	if s.Role == "" {
		s.Role = "main"
		if s.Parent != "" || inClaudeWorktree(s.Cwd) {
			s.Role = "sub"
		}
	}
	if dir == "" {
		return
	}
	g := r.gitInfo(dir, gitBy)
	s.ProjectRoot, s.RepoKey = g.project, g.key
	if s.Branch == "" {
		s.Branch = g.branch
	}
}

// gitInfo reads a directory's git info without letting a hanging disk hold up the list: the read runs in
// its own goroutine and is waited for until by (a read already in progress is waited for too). A read that
// doesn't finish in time keeps going in the background and fills the cache when it ends; meanwhile the
// directory counts as its own project ("local:" + dir). r.mu is held only to look at and update the
// caches, never while waiting.
func (r *sessionRegistry) gitInfo(dir string, by time.Time) gitInfo {
	fallback := gitInfo{project: dir, key: "local:" + dir}
	r.mu.Lock()
	if g, ok := r.git[dir]; ok {
		r.mu.Unlock()
		return g
	}
	if r.reading == nil {
		r.reading = map[string]*gitRead{}
	}
	rd := r.reading[dir]
	if rd == nil {
		if len(r.reading) >= maxGitReads {
			r.mu.Unlock()
			return fallback
		}
		rd = &gitRead{done: make(chan struct{})}
		r.reading[dir] = rd
		read := r.readGit
		if read == nil {
			read = readGitInfo
		}
		go func() {
			g, ok := fallback, false
			defer func() {
				if p := recover(); p != nil {
					log.Printf("claude-sessions: reading git info of %s panicked: %v", dir, p)
				}
				r.mu.Lock()
				delete(r.reading, dir)
				if ok {
					r.storeGitLocked(dir, g)
				}
				r.mu.Unlock()
				rd.g = g
				close(rd.done)
			}()
			g = read(dir)
			ok = true
		}()
	}
	r.mu.Unlock()
	wait := time.Until(by)
	if wait <= 0 {
		wait = time.Millisecond // past the budget: only what is answered at once
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-rd.done:
		return rd.g
	case <-t.C:
		log.Printf("claude-sessions: git info of %s didn't come in time (is its disk responding?); listed without it", dir)
		return fallback
	}
}

func (r *sessionRegistry) storeGitLocked(dir string, g gitInfo) {
	if r.git == nil || len(r.git) > 1000 {
		r.git = map[string]gitInfo{}
	}
	r.git[dir] = g
}

func inClaudeWorktree(dir string) bool {
	return strings.Contains(filepath.ToSlash(dir)+"/", "/.claude/worktrees/")
}

// ---- the desktop app's store ----

type desktopSession struct {
	SessionID      string          `json:"sessionId"`
	CliSessionID   string          `json:"cliSessionId"`
	Cwd            string          `json:"cwd"`
	OriginCwd      string          `json:"originCwd"`
	Branch         string          `json:"branch"`
	SourceBranch   string          `json:"sourceBranch"`
	Title          string          `json:"title"`
	PermissionMode string          `json:"permissionMode"`
	IsArchived     bool            `json:"isArchived"`
	SpawnedFrom    json.RawMessage `json:"spawnedFrom"`
	LastActivityAt json.RawMessage `json:"lastActivityAt"`
	CreatedAt      json.RawMessage `json:"createdAt"`
}

// readDesktopStore reads <dir>/<account>/<org>/local_*.json (deleted_* entries are left out).
// It stops with what it has once deadline has passed (checked between files).
func readDesktopStore(dir string, deadline time.Time) []ClaudeSession {
	if dir == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*", "local_*.json"))
	if len(files) > maxStoreFiles {
		files = files[:maxStoreFiles]
	}
	var out []ClaudeSession
	for _, f := range files {
		if pastDeadline(deadline) {
			log.Printf("claude-sessions: reading the desktop store takes too long; listing what was read")
			break
		}
		fi, err := os.Lstat(f)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxStoreFile {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var d desktopSession
		if json.Unmarshal(b, &d) != nil {
			continue
		}
		if d.SessionID == "" {
			d.SessionID = strings.TrimSuffix(filepath.Base(f), ".json")
		}
		s := ClaudeSession{SessionID: d.CliSessionID, AppSessionID: d.SessionID, Title: oneLineText(d.Title, 200),
			Cwd: d.Cwd, ProjectRoot: d.OriginCwd, Branch: d.Branch, Parent: rawID(d.SpawnedFrom), Source: "desktop",
			PermissionMode: d.PermissionMode, Archived: d.IsArchived, LastActivity: rawTime(d.LastActivityAt)}
		if s.LastActivity.IsZero() {
			s.LastActivity = rawTime(d.CreatedAt)
		}
		if s.LastActivity.IsZero() {
			s.LastActivity = fi.ModTime()
		}
		out = append(out, s)
	}
	return out
}

func pastDeadline(t time.Time) bool { return !t.IsZero() && time.Now().After(t) }

// rawID reads an id that may be a string or an object with an id.
func rawID(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		for _, k := range []string{"sessionId", "id", "cliSessionId"} {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
	}
	return ""
}

// rawTime reads a time given as RFC 3339 or as Unix seconds or milliseconds.
func rawTime(raw json.RawMessage) time.Time {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC()
		}
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return unixAny(n)
		}
		return time.Time{}
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		return unixAny(n)
	}
	return time.Time{}
}

func unixAny(n float64) time.Time {
	if n > 1e11 { // milliseconds
		return time.UnixMilli(int64(n)).UTC()
	}
	return time.Unix(int64(n), 0).UTC()
}

// ---- Claude Code's transcripts ----

// readTranscripts lists the newest sessions under dir (~/.claude/projects), with the working directory
// read from the first lines of each transcript. It stops with what it has once deadline has passed.
func readTranscripts(dir string, now, deadline time.Time) []ClaudeSession {
	if dir == "" {
		return nil
	}
	projects, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type tf struct {
		path string
		mod  time.Time
	}
	var all []tf
	for i, p := range projects {
		if i >= maxTranscriptDirs || pastDeadline(deadline) {
			break
		}
		if !p.IsDir() {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(dir, p.Name()))
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			fi, err := f.Info()
			if err != nil || !fi.Mode().IsRegular() || now.Sub(fi.ModTime()) > transcriptMaxAge {
				continue
			}
			all = append(all, tf{filepath.Join(dir, p.Name(), f.Name()), fi.ModTime()})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.After(all[j].mod) })
	if len(all) > maxTranscripts {
		all = all[:maxTranscripts]
	}
	out := make([]ClaudeSession, 0, len(all))
	for _, t := range all {
		if pastDeadline(deadline) {
			break
		}
		cwd, branch := transcriptHead(t.path)
		out = append(out, ClaudeSession{SessionID: strings.TrimSuffix(filepath.Base(t.path), ".jsonl"), Cwd: cwd,
			Branch: branch, Source: "transcript", LastActivity: t.mod.UTC()})
	}
	return out
}

func transcriptHead(path string) (cwd, branch string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, transcriptHeadRead))
	sc.Buffer(make([]byte, 0, 64<<10), transcriptHeadRead)
	for sc.Scan() {
		var line struct {
			Cwd       string `json:"cwd"`
			GitBranch string `json:"gitBranch"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil && line.Cwd != "" {
			return line.Cwd, line.GitBranch
		}
	}
	return "", ""
}

// ---- git, read from files ----

type gitInfo struct {
	project string // the main worktree's directory (or the directory itself without git)
	key     string // the normalized origin remote, or "local:" + project
	branch  string
}

func readGitInfo(dir string) gitInfo {
	dir = filepath.Clean(dir)
	root := dir
	var gitPath string
	for i := 0; i < 40; i++ {
		p := filepath.Join(root, ".git")
		if _, err := os.Lstat(p); err == nil {
			gitPath = p
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			break
		}
		root = parent
	}
	if gitPath == "" {
		return gitInfo{project: dir, key: "local:" + dir}
	}
	gitDir := gitPath
	if fi, err := os.Stat(gitPath); err == nil && !fi.IsDir() {
		b, _ := os.ReadFile(gitPath)
		line := strings.TrimSpace(string(b))
		if !strings.HasPrefix(line, "gitdir:") {
			return gitInfo{project: root, key: "local:" + root}
		}
		gitDir = strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(root, gitDir)
		}
	}
	common := gitDir
	if b, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		c := strings.TrimSpace(string(b))
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitDir, c)
		}
		common = filepath.Clean(c)
	}
	project := root
	if filepath.Base(common) == ".git" {
		project = filepath.Dir(common)
	}
	g := gitInfo{project: project, key: "local:" + project}
	if b, err := os.ReadFile(filepath.Join(gitDir, "HEAD")); err == nil {
		if ref, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "ref: refs/heads/"); ok {
			g.branch = ref
		}
	}
	if b, err := os.ReadFile(filepath.Join(common, "config")); err == nil {
		if u := originURL(b); u != "" {
			if k := NormalizeRemote(u); k != "" {
				g.key = k
			}
		}
	}
	return g
}

// originURL reads [remote "origin"] url from a git config.
func originURL(cfg []byte) string {
	in := false
	for _, line := range bytes.Split(cfg, []byte("\n")) {
		l := strings.TrimSpace(string(line))
		if strings.HasPrefix(l, "[") {
			in = strings.EqualFold(strings.Join(strings.Fields(l), " "), `[remote "origin"]`)
			continue
		}
		if !in {
			continue
		}
		if k, v, ok := strings.Cut(l, "="); ok && strings.EqualFold(strings.TrimSpace(k), "url") {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// NormalizeRemote makes one key of the forms of a git remote: https://github.com/a/b.git,
// git@github.com:a/b and ssh://git@github.com/a/b all become github.com/a/b.
func NormalizeRemote(u string) string {
	u = strings.TrimSpace(u)
	host, path := "", ""
	if strings.Contains(u, "://") {
		p, err := url.Parse(u)
		if err != nil || p.Host == "" {
			return ""
		}
		host, path = p.Hostname(), p.Path
	} else if at, rest, ok := strings.Cut(u, ":"); ok && !strings.HasPrefix(u, "/") {
		if i := strings.LastIndexByte(at, '@'); i >= 0 {
			at = at[i+1:]
		}
		host, path = at, rest
	} else {
		return ""
	}
	path = strings.Trim(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/")
	if host == "" || path == "" {
		return ""
	}
	return strings.ToLower(host) + "/" + path
}

func oneLineText(s string, n int) string {
	s = strings.Join(strings.Fields(cleanMessage(s)), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
