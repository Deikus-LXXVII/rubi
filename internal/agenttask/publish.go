package agenttask

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var objectRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// Publisher never executes Git in the agent-owned checkout. Only a private bare
// object snapshot, our configuration and the single fixed task ref reach Git.
type Publisher struct {
	Store  Store
	Helper string
	client *http.Client
	api    string
	remote func(Target) string // package-private, local fixture transport only
}

func (p Publisher) httpClient() *http.Client {
	c := http.DefaultClient
	if p.client != nil {
		c = p.client
	}
	clone := *c
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("task: redirects refused") }
	clone.Timeout = 20 * time.Second
	return &clone
}
func (p Publisher) apiBase() string {
	if p.api != "" {
		return p.api
	}
	return "https://api.github.com"
}
func (p Publisher) remoteURL(t Target) string {
	if p.remote != nil {
		return p.remote(t)
	}
	return "https://github.com/" + t.Repository + ".git"
}
func trustedGit(ctx context.Context, dir string, args ...string) (string, error) {
	return publishGit(ctx, dir, "", "", false, args...)
}
func publishGit(ctx context.Context, dir, helper, token string, local bool, args ...string) (string, error) {
	prefix := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.attributesFile=/dev/null", "-c", "credential.helper=", "-c", "http.followRedirects=false", "-c", "http.sslVerify=true", "-c", "protocol.ext.allow=never", "-C", dir}
	if helper != "" {
		prefix = append(prefix, "-c", "credential.helper=!"+shellQuote(helper)+" --git-credential-helper")
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/git", append(prefix, args...)...)
	protocol := "https"
	if local {
		protocol = "file"
	}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=" + protocol}
	cmd.WaitDelay = 2 * time.Second
	var out cappedBuffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if helper != "" {
		if len(token) == 0 || len(token) > 4096 || strings.ContainsAny(token, "\r\n\x00") {
			return "", errors.New("task: missing or invalid publishing credential")
		}
		r, w, err := os.Pipe()
		if err != nil {
			return "", errors.New("task: cannot create credential pipe")
		}
		cmd.ExtraFiles = []*os.File{r}
		done := make(chan struct{})
		go func() { defer close(done); _, _ = io.WriteString(w, token); w.Close() }()
		defer func() { r.Close(); w.Close(); <-done }()
	}
	if err := cmd.Run(); err != nil {
		return "", errors.New("task: trusted Git operation failed (details suppressed)")
	}
	return strings.TrimSpace(out.String()), nil
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// CredentialHelper serves only Git's get operation over inherited FD 3. Neither
// config, argv, environment nor project files contain the credential.
func CredentialHelper(op string, credential io.Reader, request io.Reader, response io.Writer) error {
	if op != "get" {
		return nil
	}
	scan := bufio.NewScanner(io.LimitReader(request, 8193))
	protocol, host := "", ""
	for scan.Scan() {
		s := scan.Text()
		if s == "" {
			break
		}
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return errors.New("invalid credential request")
		}
		switch k {
		case "protocol":
			protocol = v
		case "host":
			host = v
		}
	}
	if scan.Err() != nil || protocol != "https" || host != "github.com" {
		return errors.New("credential destination refused")
	}
	b, err := io.ReadAll(io.LimitReader(credential, 4097))
	if err != nil || len(b) == 0 || len(b) > 4096 || bytes.ContainsAny(b, "\r\n\x00") {
		return errors.New("credential pipe unavailable")
	}
	_, err = fmt.Fprintf(response, "username=x-access-token\npassword=%s\n\n", b)
	return err
}

func snapshot(ctx context.Context, home string, s State) (dir, commit string, err error) {
	parent := filepath.Join(home, "t3", "task-snapshots")
	if err = privateDir(parent); err != nil {
		return "", "", err
	}
	final := filepath.Join(parent, s.ID)
	if st, e := os.Lstat(final); e == nil {
		if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
			return "", "", errors.New("task: unsafe publication snapshot")
		}
		existing, e := trustedGit(ctx, final, "rev-parse", "refs/heads/"+s.Workspace.Branch)
		if !objectRE.MatchString(existing) {
			return "", "", errors.New("task: invalid frozen commit")
		}
		if _, e := trustedGit(ctx, final, "fsck", "--strict", "--full", "--no-reflogs"); e != nil {
			return "", "", errors.New("task: invalid frozen objects")
		}
		if _, e := trustedGit(ctx, final, "merge-base", "--is-ancestor", s.Workspace.BaseCommit, existing); e != nil || existing == s.Workspace.BaseCommit {
			return "", "", errors.New("task: invalid frozen ancestry")
		}
		if e != nil || (s.Commit != "" && s.Commit != existing) {
			return "", "", errors.New("task: frozen commit changed")
		}
		return final, existing, nil
	}
	w, err := Resolve(home, s.ID, s.Workspace.Project, s.Workspace.Role, s.Workspace.SourceHash)
	if err != nil {
		return "", "", err
	}
	root, err := os.OpenRoot(w.Dir)
	if err != nil {
		return "", "", errors.New("task: workspace root unavailable")
	}
	defer root.Close()
	gitroot, err := root.OpenRoot(".git")
	if err != nil {
		return "", "", errors.New("task: unsafe Git database")
	}
	defer gitroot.Close()
	head, err := readMetadata(gitroot, "HEAD", 4096)
	if err != nil || strings.TrimSpace(string(head)) != "ref: refs/heads/"+w.Branch {
		return "", "", errors.New("task: HEAD is not the assigned task branch")
	}
	ref := filepath.Join("refs", "heads", filepath.FromSlash(w.Branch))
	b, err := readMetadata(gitroot, ref, 4096)
	if os.IsNotExist(err) {
		packed, e := readMetadata(gitroot, "packed-refs", 1<<20)
		if e != nil {
			return "", "", errors.New("task: assigned ref missing")
		}
		for _, line := range strings.Split(string(packed), "\n") {
			parts := strings.Fields(line)
			if len(parts) == 2 && parts[1] == "refs/heads/"+w.Branch {
				b = []byte(parts[0])
				err = nil
				break
			}
		}
	}
	commit = strings.TrimSpace(string(b))
	if err != nil || !objectRE.MatchString(commit) {
		return "", "", errors.New("task: invalid assigned commit")
	}
	dir, err = os.MkdirTemp(parent, ".freeze-")
	if err != nil {
		return "", "", err
	}
	temporary := dir
	defer os.RemoveAll(temporary)
	if _, err = trustedGit(ctx, dir, "init", "--bare", "--template="); err != nil {
		return "", "", err
	}
	objects, err := gitroot.OpenRoot("objects")
	if err != nil {
		return "", "", errors.New("task: object database unavailable")
	}
	defer objects.Close()
	total := int64(0)
	count := 0
	var copyTree func(string) error
	copyTree = func(rel string) error {
		count++
		if count > 100000 || strings.Count(rel, string(filepath.Separator)) > 100 {
			return errors.New("task: snapshot directory limit exceeded")
		}
		f, e := objects.OpenFile(rel, os.O_RDONLY|fsx.NonBlock, 0)
		if e != nil {
			return errors.New("task: cannot open snapshot directory")
		}
		entries, e := f.ReadDir(-1)
		f.Close()
		if e != nil {
			return e
		}
		for _, entry := range entries {
			name := filepath.Join(rel, entry.Name())
			if name == filepath.Join("info", "alternates") || name == filepath.Join("info", "http-alternates") {
				return errors.New("task: external object store refused")
			}
			info, e := objects.Lstat(name)
			if e != nil {
				return e
			}
			dst := filepath.Join(dir, "objects", name)
			if info.IsDir() {
				if e = os.MkdirAll(dst, 0700); e != nil {
					return e
				}
				if e = copyTree(name); e != nil {
					return e
				}
				continue
			}
			if !info.Mode().IsRegular() {
				return errors.New("task: nonregular object refused")
			}
			count++
			if count > 100000 || info.Size() > 512<<20-total {
				return errors.New("task: snapshot limit exceeded")
			}
			in, e := objects.OpenFile(name, os.O_RDONLY|fsx.NonBlock, 0)
			if e != nil {
				return e
			}
			if st, e := in.Stat(); e != nil || !st.Mode().IsRegular() {
				in.Close()
				return errors.New("task: nonregular object refused")
			}
			out, e := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e != nil {
				in.Close()
				return e
			}
			n, copyErr := io.Copy(out, io.LimitReader(in, (512<<20)-total+1))
			total += n
			e = copyErr
			if total > 512<<20 {
				e = errors.New("task: snapshot limit exceeded")
			}
			in.Close()
			e = errors.Join(e, out.Sync(), out.Close())
			if e != nil {
				return e
			}
		}
		return nil
	}
	if err = copyTree("."); err != nil {
		return "", "", err
	}
	if _, err = trustedGit(ctx, dir, "update-ref", "refs/heads/"+w.Branch, commit); err != nil {
		return "", "", err
	}
	if _, err = trustedGit(ctx, dir, "fsck", "--strict", "--full", "--no-reflogs"); err != nil {
		return "", "", errors.New("task: corrupt snapshot refused")
	}
	if _, err = trustedGit(ctx, dir, "merge-base", "--is-ancestor", w.BaseCommit, commit); err != nil {
		return "", "", errors.New("task: commit does not descend from approved base")
	}
	if commit == w.BaseCommit {
		return "", "", errors.New("task: no new committed change to publish")
	}
	if err = syncTree(dir); err != nil {
		return "", "", err
	}
	if err = os.Rename(dir, final); err != nil {
		return "", "", err
	}
	d, e := os.Open(parent)
	if e != nil {
		return "", "", e
	}
	e = errors.Join(d.Sync(), d.Close())
	if e != nil {
		return "", "", e
	}
	dir = final
	return final, commit, nil
}
func (p Publisher) request(ctx context.Context, token, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, e := json.Marshal(in)
		if e != nil {
			return e
		}
		body = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, p.apiBase()+path, body)
	if e != nil {
		return errors.New("task: invalid GitHub request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	res, e := p.httpClient().Do(req)
	if e != nil {
		return errors.New("task: GitHub request failed (details suppressed)")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("task: GitHub refused request (%d)", res.StatusCode)
	}
	if out != nil {
		if e = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out); e != nil {
			return errors.New("task: invalid GitHub response")
		}
	}
	return nil
}

type pull struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
	Head   struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			Full string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func validPull(pr pull, s State) bool {
	expected := fmt.Sprintf("https://github.com/%s/pull/%d", s.Spec.Target.Repository, pr.Number)
	return pr.Number > 0 && pr.URL == expected && pr.Head.Ref == s.Workspace.Branch && pr.Head.SHA == s.Commit && strings.EqualFold(pr.Head.Repo.Full, s.Spec.Target.Repository) && pr.Base.Ref == s.Spec.Target.Base
}
func (p Publisher) Publish(ctx context.Context, id, token string) (out State, err error) {
	err = p.Store.With(ctx, id, func(tx *Tx) error {
		s := &tx.State
		if s.ID == "" {
			return os.ErrNotExist
		}
		if s.Phase.Terminal() {
			out = *s
			return nil
		}
		if s.Phase != Waiting && s.Phase != Publishing && s.Phase != Pushed {
			return errors.New("task: task must finish its active turns before publication")
		}
		if s.Pending != nil {
			return errors.New("task: unresolved native command prevents publication")
		}
		if s.Phase == Waiting {
			s.Phase = Publishing
			if e := tx.Save(); e != nil {
				return e
			}
		}
		dir := filepath.Join(p.Store.Home, "t3", "task-snapshots", s.ID)
		if s.Commit == "" {
			var e error
			dir, s.Commit, e = snapshot(ctx, p.Store.Home, *s)
			if e != nil {
				s.Phase = Failed
				s.Error = "task: snapshot validation failed"
				_ = tx.Save()
				out = *s
				return e
			}
			if e = tx.Save(); e != nil {
				return e
			}
		} else {
			if _, e := trustedGit(ctx, dir, "fsck", "--strict", "--full", "--no-reflogs"); e != nil {
				return errors.New("task: frozen snapshot unavailable")
			}
		}
		if s.Spec.Target == nil {
			s.Phase = Finished
			if e := tx.Save(); e != nil {
				return e
			}
			out = *s
			return nil
		}
		if e := s.Spec.Target.Validate(); e != nil {
			return e
		}
		if token == "" {
			return errors.New("task: publishing credential unavailable")
		}
		var repo struct {
			Default string `json:"default_branch"`
		}
		path := "/repos/" + s.Spec.Target.Repository
		if e := p.request(ctx, token, "GET", path, nil, &repo); e != nil {
			return e
		}
		if repo.Default == "" || s.Spec.Target.Base == s.Workspace.Branch || repo.Default == s.Workspace.Branch || s.Workspace.Branch != "rubi/"+s.ID {
			return errors.New("task: default or foreign branch publication refused")
		}
		if s.Phase != Pushed {
			remote := p.remoteURL(*s.Spec.Target)
			helper := p.Helper
			if p.remote != nil {
				helper = ""
			} else if !filepath.IsAbs(helper) {
				return errors.New("task: credential helper unavailable")
			}
			heads, e := publishGit(ctx, dir, helper, token, p.remote != nil, "ls-remote", remote, "refs/heads/"+s.Workspace.Branch)
			if e != nil {
				return e
			}
			if heads != "" && strings.Fields(heads)[0] != s.Commit {
				return errors.New("task: remote task branch changed; refusing overwrite")
			}
			if heads == "" {
				if e = pushBranch(ctx, dir, remote, helper, token, p.remote != nil, s.Workspace.Branch, s.Commit); e != nil {
					return e
				}
			}
			s.Phase = Pushed
			if e = tx.Save(); e != nil {
				return e
			}
		}
		q := url.Values{"state": {"all"}, "head": {strings.Split(s.Spec.Target.Repository, "/")[0] + ":" + s.Workspace.Branch}, "base": {s.Spec.Target.Base}, "per_page": {"100"}}
		var prs []pull
		if e := p.request(ctx, token, "GET", path+"/pulls?"+q.Encode(), nil, &prs); e != nil {
			return e
		}
		if len(prs) > 1 {
			return errors.New("task: ambiguous existing pull requests")
		}
		var pr pull
		if len(prs) == 1 {
			pr = prs[0]
		} else {
			body := map[string]any{"head": s.Workspace.Branch, "base": s.Spec.Target.Base, "title": "Rubi task " + s.ID, "body": "Committed task snapshot: " + s.Commit + ".\n\nNo automatic merge is requested."}
			if e := p.request(ctx, token, "POST", path+"/pulls", body, &pr); e != nil {
				return e
			}
		}
		if !validPull(pr, *s) {
			return errors.New("task: GitHub pull request binding mismatch")
		}
		s.PR = pr.URL
		s.Phase = Finished
		if e := tx.Save(); e != nil {
			return e
		}
		out = *s
		return nil
	})
	return
}

type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := (1 << 20) - b.Len()
	if left > 0 {
		_, _ = b.Buffer.Write(p[:min(left, n)])
	}
	return n, nil
}
func syncTree(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		return errors.Join(f.Sync(), f.Close())
	})
}

// An expected-absent lease is a conditional CREATE, never permission to replace
// an existing branch. It closes the ls-remote/advertisement fast-forward race.
func pushBranch(ctx context.Context, dir, remote, helper, token string, local bool, branch, commit string) error {
	id, ok := strings.CutPrefix(branch, "rubi/")
	if !ok || !ValidID(id) || !objectRE.MatchString(commit) {
		return errors.New("task: foreign push ref refused")
	}
	_, err := publishGit(ctx, dir, helper, token, local, "push", "--porcelain", "--force-with-lease=refs/heads/"+branch+":", "--", remote, commit+":refs/heads/"+branch)
	return err
}
