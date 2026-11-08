// Package agentsandbox launches provider CLIs through Anthropic Sandbox Runtime.
package agentsandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Policy is supplied by the trusted session resolver, never by the provider CLI.
// Every path must already exist. RuntimeRead includes the installed CLI, srt,
// launcher and dependencies where they live underneath a denied root.
type Policy struct {
	Projects                    []string
	Login, Tmp, Home            string
	SensitiveRoots, RuntimeRead []string
	AllowedDomains              []string
}

type Filesystem struct {
	DenyRead   []string `json:"denyRead"`
	AllowRead  []string `json:"allowRead"`
	AllowWrite []string `json:"allowWrite"`
	DenyWrite  []string `json:"denyWrite"`
}
type Network struct {
	AllowedDomains []string `json:"allowedDomains"`
	DeniedDomains  []string `json:"deniedDomains"`
}
type Settings struct {
	Filesystem Filesystem `json:"filesystem"`
	Network    Network    `json:"network"`
}

func canonical(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("sandbox path must be absolute")
	}
	return filepath.EvalSymlinks(filepath.Clean(p))
}
func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Build rejects broad read exceptions: srt can remount nested writable paths
// read-only when an allowRead ancestor covers them (including private tmp).
// Protected paths are settings storage, srt, launcher and all runtime paths.
func Build(p Policy, protected []string) (Settings, error) {
	s := Settings{Filesystem: Filesystem{DenyWrite: []string{}}, Network: Network{AllowedDomains: []string{}, DeniedDomains: []string{}}}
	if len(p.Projects) == 0 {
		return s, fmt.Errorf("sandbox requires a project")
	}
	var err error
	add := func(dst *[]string, paths ...string) error {
		for _, path := range paths {
			v, e := canonical(path)
			if e != nil {
				return e
			}
			if v == string(filepath.Separator) {
				return fmt.Errorf("sandbox root path is not permitted")
			}
			*dst = append(*dst, v)
		}
		return nil
	}
	if err = add(&s.Filesystem.DenyRead, p.Home); err != nil {
		return s, err
	}
	for _, root := range []string{"/home", "/Users", "/workspace", "/cursor", "/root"} {
		if _, e := os.Stat(root); e == nil {
			if e = add(&s.Filesystem.DenyRead, root); e != nil {
				return s, e
			}
		}
	}
	if err = add(&s.Filesystem.DenyRead, p.SensitiveRoots...); err != nil {
		return s, err
	}
	if err = add(&s.Filesystem.AllowWrite, append(append([]string{}, p.Projects...), p.Login, p.Tmp)...); err != nil {
		return s, err
	}
	if err = add(&s.Filesystem.AllowRead, append(append([]string{}, p.Projects...), p.Login)...); err != nil {
		return s, err
	}
	if err = add(&s.Filesystem.AllowRead, p.RuntimeRead...); err != nil {
		return s, err
	}
	home, _ := canonical(p.Home)
	for _, w := range s.Filesystem.AllowWrite {
		if within(w, home) {
			return s, fmt.Errorf("write access may not cover HOME")
		}
		for _, r := range s.Filesystem.AllowRead {
			if r != w && within(r, w) {
				return s, fmt.Errorf("allowRead ancestor of writable path would cause EROFS")
			}
		}
	}
	tmp, _ := canonical(p.Tmp)
	for _, r := range s.Filesystem.AllowRead {
		if within(r, tmp) {
			return s, fmt.Errorf("private tmp must not be covered by allowRead")
		}
	}
	for _, path := range append(append([]string{}, protected...), p.RuntimeRead...) {
		v, e := canonical(path)
		if e != nil {
			return s, e
		}
		for _, w := range s.Filesystem.AllowWrite {
			if within(w, v) || within(v, w) {
				return s, fmt.Errorf("sandbox runtime/settings overlaps writable path")
			}
		}
	}
	for _, d := range p.AllowedDomains {
		if d == "" || strings.ContainsAny(d, "/:@ \t\r\n") || d == "*" {
			return s, fmt.Errorf("invalid sandbox allowed domain")
		}
		s.Network.AllowedDomains = append(s.Network.AllowedDomains, d)
	}
	return s, nil
}
