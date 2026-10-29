package home

// The registry of Claude Code sessions on this computer (claude.sessions). It reads, never writes:
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
)

// ClaudeSession is one Claude Code session as claude.sessions reports it. Title comes from the user or
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

type sessionRegistry struct {
	mu        sync.Mutex
	live      map[string]*liveSession
	scanned   []ClaudeSession
	scannedAt time.Time
	git       map[string]gitInfo
}

// touch records what a hook said about a session.
func (r *sessionRegistry) touch(id, cwd, transcript, mode, state string, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.live[id]; s != nil {
		return s.transcript
	}
	return ""
}

// list merges the stores and the hooks. desktop and transcripts are directories ("" to skip).
func (r *sessionRegistry) list(desktop, transcripts string, now time.Time) []ClaudeSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.scanned == nil || now.Sub(r.scannedAt) > sessionScanEvery {
		r.scanned = r.scanLocked(desktop, transcripts, now)
		r.scannedAt = now
	}
	out := make([]ClaudeSession, 0, len(r.scanned)+len(r.live))
	seen := map[string]bool{}
	for _, s := range r.scanned {
		if l := r.live[s.SessionID]; l != nil && s.SessionID != "" {
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
	for id, l := range r.live {
		if seen[id] {
			continue
		}
		s := ClaudeSession{SessionID: id, Cwd: l.cwd, Source: "hook", State: l.state, PermissionMode: l.mode, LastActivity: l.last}
		r.fillProjectLocked(&s, l.cwd)
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastActivity.After(out[j].LastActivity) })
	return out
}

func (r *sessionRegistry) scanLocked(desktop, transcripts string, now time.Time) []ClaudeSession {
	var out []ClaudeSession
	seen := map[string]bool{}
	for _, s := range readDesktopStore(desktop) {
		r.fillProjectLocked(&s, s.ProjectRoot)
		if s.SessionID != "" {
			seen[s.SessionID] = true
		}
		out = append(out, s)
	}
	for _, s := range readTranscripts(transcripts, now) {
		if seen[s.SessionID] {
			continue
		}
		r.fillProjectLocked(&s, s.Cwd)
		out = append(out, s)
	}
	return out
}

// fillProjectLocked sets the project from dir (the origin directory for desktop sessions, otherwise the
// working directory) and the role from the working directory.
func (r *sessionRegistry) fillProjectLocked(s *ClaudeSession, dir string) {
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
	if r.git == nil {
		r.git = map[string]gitInfo{}
	}
	g, ok := r.git[dir]
	if !ok {
		g = readGitInfo(dir)
		if len(r.git) > 1000 {
			r.git = map[string]gitInfo{}
		}
		r.git[dir] = g
	}
	s.ProjectRoot, s.RepoKey = g.project, g.key
	if s.Branch == "" {
		s.Branch = g.branch
	}
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
func readDesktopStore(dir string) []ClaudeSession {
	if dir == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*", "local_*.json"))
	if len(files) > maxStoreFiles {
		files = files[:maxStoreFiles]
	}
	var out []ClaudeSession
	for _, f := range files {
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
// read from the first lines of each transcript.
func readTranscripts(dir string, now time.Time) []ClaudeSession {
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
		if i >= maxTranscriptDirs {
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
