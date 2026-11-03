// Package t3bump finds the newest T3 Code nightly and moves Rubi's pin to it (docs/plans/t3-integration.md S6). It reads
// the GitHub releases API and the npm registry, checks what it downloads against what the registry and the release say, and
// writes t3/upstream.json; it never merges anything. The contract tests and the report of changed tool schemas are the
// review that a person does before taking the bump (the workflow opens a pull request, nothing more).
package t3bump

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Where the real sources are. Tests point the Client at stand-ins.
const (
	DefaultAPI = "https://api.github.com"
	DefaultNPM = "https://registry.npmjs.org"
	Repo       = "pingdotgg/t3code"
	Package    = "t3"
)

// Platforms are the ones Rubi Home installs on, in the order the pin lists them.
var Platforms = []string{"darwin-arm64", "linux-arm64", "linux-x64"}

var tagRE = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)-nightly\.(\d{8})\.(\d+)$`)

// Release is a nightly the API lists.
type Release struct {
	Tag     string
	Version string // the tag without the v
	Date    string // YYYYMMDD
	N       int
	major   [3]int
	// SumsURL is where the release's SHA256SUMS asset is.
	SumsURL string
}

// Less orders nightlies: by date, then by the number after it, then by the version triple.
func (r Release) Less(o Release) bool {
	if r.Date != o.Date {
		return r.Date < o.Date
	}
	if r.N != o.N {
		return r.N < o.N
	}
	for i := range r.major {
		if r.major[i] != o.major[i] {
			return r.major[i] < o.major[i]
		}
	}
	return false
}

// ParseTag reads a nightly tag (vX.Y.Z-nightly.YYYYMMDD.N).
func ParseTag(tag string) (Release, bool) {
	m := tagRE.FindStringSubmatch(tag)
	if m == nil {
		return Release{}, false
	}
	r := Release{Tag: tag, Version: tag[1:], Date: m[4]}
	r.N, _ = strconv.Atoi(m[5])
	for i := 0; i < 3; i++ {
		r.major[i], _ = strconv.Atoi(m[i+1])
	}
	if _, err := time.Parse("20060102", r.Date); err != nil {
		return Release{}, false
	}
	return r, true
}

// Client reads the two registries.
type Client struct {
	API, NPM string
	// Token, when set, is sent to the GitHub API (never to another host).
	Token string
	HC    *http.Client
}

func (c *Client) hc() *http.Client {
	if c.HC != nil {
		return c.HC
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

// checkURL allows https, and plain http only to a loopback address (the tests' stand-ins).
func checkURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bad address %q", clip(raw, 80))
	}
	if u.Scheme == "https" {
		return u, nil
	}
	if u.Scheme == "http" {
		if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() || u.Hostname() == "localhost" {
			return u, nil
		}
	}
	return nil, fmt.Errorf("refusing %s: only https (or http to this computer) is allowed", u.Redacted())
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

const bodyMax = 1 << 30

// get fetches a URL. The token goes only to the API's own host.
func (c *Client) get(ctx context.Context, raw string, max int64, sendToken bool) ([]byte, error) {
	u, err := checkURL(raw)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "rubi-t3-bump")
	if sendToken && c.Token != "" {
		if api, err := url.Parse(c.API); err == nil && api.Host == u.Host {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
	}
	hc := *c.hc()
	hc.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if _, err := checkURL(r.URL.String()); err != nil || len(via) > 5 {
			return errors.New("refusing a redirect")
		}
		r.Header.Del("Authorization") // a redirect goes to another host (release assets): no token there
		return nil
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", u.Host+u.Path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", u.Host+u.Path, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", u.Host+u.Path, max)
	}
	return b, nil
}

type ghRelease struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Newest finds the newest published nightly that has a SHA256SUMS asset.
func (c *Client) Newest(ctx context.Context) (Release, error) {
	var best Release
	found := false
	for page := 1; page <= 5; page++ {
		b, err := c.get(ctx, fmt.Sprintf("%s/repos/%s/releases?per_page=100&page=%d", strings.TrimRight(c.API, "/"), Repo, page), 32<<20, true)
		if err != nil {
			return Release{}, err
		}
		var rels []ghRelease
		if err := json.Unmarshal(b, &rels); err != nil {
			return Release{}, errors.New("the releases list is not what was expected")
		}
		for _, g := range rels {
			r, ok := ParseTag(g.Tag)
			if !ok || g.Draft {
				continue
			}
			for _, a := range g.Assets {
				if a.Name == "SHA256SUMS" {
					r.SumsURL = a.URL
				}
			}
			if r.SumsURL == "" {
				continue
			}
			if !found || best.Less(r) {
				best, found = r, true
			}
		}
		if len(rels) < 100 {
			break
		}
	}
	if !found {
		return Release{}, errors.New("no published nightly with a SHA256SUMS asset was found")
	}
	return best, nil
}

// Pin is what upstream.json takes from a release.
type Pin struct {
	Version   string
	Tag       string
	Commit    string
	Integrity string
	Archives  map[string]string // platform -> SHA-256 (hex) of the release archive
}

var hexRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseSums reads a SHA256SUMS file and returns the SHA-256 of each supported platform's archive for the version. Every
// platform must be there.
func ParseSums(text, version string) (map[string]string, error) {
	want := map[string]string{}
	for _, p := range Platforms {
		want["t3-"+version+"-"+p+".tar.gz"] = p
	}
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		name := strings.TrimPrefix(f[1], "*")
		if p, ok := want[name]; ok {
			sum := strings.ToLower(f[0])
			if !hexRE.MatchString(sum) {
				return nil, fmt.Errorf("SHA256SUMS: the sum of %s is not a SHA-256", name)
			}
			if prev, dup := out[p]; dup && prev != sum {
				return nil, fmt.Errorf("SHA256SUMS lists %s twice with different sums", name)
			}
			out[p] = sum
		}
	}
	for _, p := range Platforms {
		if out[p] == "" {
			return nil, fmt.Errorf("SHA256SUMS has no archive for %s", p)
		}
	}
	return out, nil
}

type packument struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Dist    struct {
		Tarball   string `json:"tarball"`
		Integrity string `json:"integrity"`
	} `json:"dist"`
}

// Fetch collects the pin of a release: the archives' sums from the release, the integrity from npm (checked by hashing the
// tarball it points to) and the commit the tag names.
func (c *Client) Fetch(ctx context.Context, r Release) (Pin, error) {
	sums, err := c.get(ctx, r.SumsURL, 1<<20, false)
	if err != nil {
		return Pin{}, err
	}
	arch, err := ParseSums(string(sums), r.Version)
	if err != nil {
		return Pin{}, err
	}
	b, err := c.get(ctx, fmt.Sprintf("%s/%s/%s", strings.TrimRight(c.NPM, "/"), Package, url.PathEscape(r.Version)), 8<<20, false)
	if err != nil {
		return Pin{}, fmt.Errorf("npm: %w", err)
	}
	var pk packument
	if err := json.Unmarshal(b, &pk); err != nil || pk.Name != Package || pk.Version != r.Version {
		return Pin{}, errors.New("npm: the registry did not answer with this version of t3")
	}
	if !strings.HasPrefix(pk.Dist.Integrity, "sha512-") {
		return Pin{}, errors.New("npm: the version has no sha512 integrity")
	}
	tgz, err := c.get(ctx, pk.Dist.Tarball, 64<<20, false)
	if err != nil {
		return Pin{}, fmt.Errorf("npm tarball: %w", err)
	}
	h := sha512.Sum512(tgz)
	if got := "sha512-" + base64.StdEncoding.EncodeToString(h[:]); got != pk.Dist.Integrity {
		return Pin{}, errors.New("npm: the tarball does not match the integrity the registry gives")
	}
	cb, err := c.get(ctx, fmt.Sprintf("%s/repos/%s/commits/%s", strings.TrimRight(c.API, "/"), Repo, url.PathEscape(r.Tag)), 8<<20, true)
	if err != nil {
		return Pin{}, fmt.Errorf("the tag's commit: %w", err)
	}
	var cm struct {
		SHA string `json:"sha"`
	}
	if json.Unmarshal(cb, &cm) != nil || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(cm.SHA) {
		return Pin{}, errors.New("the tag's commit is not what was expected")
	}
	return Pin{Version: r.Version, Tag: r.Tag, Commit: cm.SHA, Integrity: pk.Dist.Integrity, Archives: arch}, nil
}

// Upstream mirrors t3/upstream.json, in its order.
type Upstream struct {
	Package   string `json:"package"`
	Version   string `json:"version"`
	Tag       string `json:"tag"`
	Commit    string `json:"commit"`
	Integrity string `json:"integrity"`
	License   string `json:"license"`
	Checked   string `json:"checked"`
	Runtime   struct {
		Note   string            `json:"note"`
		SHA256 map[string]string `json:"sha256"`
	} `json:"runtime"`
	Node json.RawMessage `json:"node"`
}

// Read loads upstream.json from the repository root.
func Read(root string) (Upstream, error) {
	var u Upstream
	b, err := os.ReadFile(filepath.Join(root, "t3", "upstream.json"))
	if err != nil {
		return u, err
	}
	if err := json.Unmarshal(b, &u); err != nil {
		return u, err
	}
	return u, nil
}

// Apply returns upstream with the pin and the date put in. Node and the license are kept.
func Apply(u Upstream, p Pin, date string) Upstream {
	u.Version, u.Tag, u.Commit, u.Integrity, u.Checked = p.Version, p.Tag, p.Commit, p.Integrity, date
	sums := map[string]string{}
	for k, v := range p.Archives {
		sums[k] = v
	}
	u.Runtime.SHA256 = sums
	return u
}

// Marshal renders upstream.json the way the file is kept: indented by two spaces, one trailing newline.
func Marshal(u Upstream) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(u); err != nil {
		return nil, err
	}
	// the node block is kept as it was (the encoder re-indents a RawMessage)
	return buf.Bytes(), nil
}

// Write stores upstream.json.
func Write(root string, u Upstream) error {
	b, err := Marshal(u)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "t3", "upstream.json"), b, 0o644)
}

// ---- the tool schemas ----

// Change is one difference between two input schemas of a tool.
type Change struct {
	Tool string
	Path string // where in the schema, like properties.message.maxLength
	Kind string // added, removed or changed
	Old  string
	New  string
}

func (c Change) String() string {
	switch c.Kind {
	case "added":
		return fmt.Sprintf("`%s` `%s` added: %s", c.Tool, c.Path, c.New)
	case "removed":
		return fmt.Sprintf("`%s` `%s` removed (was %s)", c.Tool, c.Path, c.Old)
	}
	return fmt.Sprintf("`%s` `%s` changed: %s -> %s", c.Tool, c.Path, c.Old, c.New)
}

// DiffTools compares the schemas of the tools Rubi uses. A tool that is missing from the new set is a removal of the whole
// tool; one missing from the old set is an addition.
func DiffTools(old, cur map[string]any, used []string) []Change {
	var out []Change
	tools := append([]string(nil), used...)
	sort.Strings(tools)
	for _, name := range tools {
		o, hasOld := old[name]
		n, hasNew := cur[name]
		switch {
		case !hasOld && !hasNew:
		case !hasNew:
			out = append(out, Change{Tool: name, Path: "(the tool)", Kind: "removed", Old: compact(o)})
		case !hasOld:
			out = append(out, Change{Tool: name, Path: "(the tool)", Kind: "added", New: compact(n)})
		default:
			diffValue(name, "", o, n, &out)
		}
	}
	return out
}

func compact(v any) string {
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

func diffValue(tool, path string, o, n any, out *[]Change) {
	switch ov := o.(type) {
	case map[string]any:
		nv, ok := n.(map[string]any)
		if !ok {
			*out = append(*out, Change{tool, pathOr(path), "changed", compact(o), compact(n)})
			return
		}
		keys := map[string]bool{}
		for k := range ov {
			keys[k] = true
		}
		for k := range nv {
			keys[k] = true
		}
		var sorted []string
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			p := k
			if path != "" {
				p = path + "." + k
			}
			a, hasA := ov[k]
			b, hasB := nv[k]
			switch {
			case !hasB:
				*out = append(*out, Change{tool, p, "removed", compact(a), ""})
			case !hasA:
				*out = append(*out, Change{tool, p, "added", "", compact(b)})
			default:
				diffValue(tool, p, a, b, out)
			}
		}
	default:
		if compact(o) != compact(n) {
			*out = append(*out, Change{tool, pathOr(path), "changed", compact(o), compact(n)})
		}
	}
}

func pathOr(p string) string {
	if p == "" {
		return "(schema)"
	}
	return p
}
