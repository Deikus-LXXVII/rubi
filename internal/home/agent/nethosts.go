package agent

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// NetFile is where the hosts the owner approved (net.ask) are kept, in the helper's directory: per project
// directory for the legacy role, and per role id for an isolated role (a role never gets another role's hosts).
const NetFile = "agent-net.json"

const maxApprovedHosts = 100

// netState keeps approved hosts by project directory or by role (0600, atomic). Nothing here comes from the
// project's own files: only from an approval the plugin gave. Keys are a directory (an absolute path) or
// "role:<id>" (roleNetKey).
type netState struct {
	path string
	mu   sync.Mutex
	d    map[string][]string
}

// roleNetKey is the key of an isolated role's approvals.
func roleNetKey(id string) string { return "role:" + id }

func newNetState(dir string) *netState {
	n := &netState{d: map[string][]string{}}
	if dir == "" {
		return n
	}
	n.path = filepath.Join(dir, NetFile)
	f, err := os.Open(n.path)
	if err != nil {
		return n
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, 1<<20))
	var file struct {
		Projects map[string][]string `json:"projects"`
		Roles    map[string][]string `json:"roles"`
	}
	if json.Unmarshal(b, &file) != nil {
		log.Printf("agent: %s is unusable; starting without approved hosts", n.path)
		return n
	}
	load := func(key string, hosts []string) {
		for _, h := range hosts {
			if v := normalizeHost(h); v != "" && !refusedHost(v) && len(n.d[key]) < maxApprovedHosts {
				n.d[key] = append(n.d[key], v)
			}
		}
	}
	for dir, hosts := range file.Projects {
		load(dir, hosts)
	}
	for id, hosts := range file.Roles {
		load(roleNetKey(id), hosts)
	}
	return n
}

func (n *netState) hosts(dir string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.d[dir]...)
}

// add records an approved host; false when the project is at its cap.
func (n *netState) add(dir, host string) bool {
	h := normalizeHost(host)
	if h == "" || refusedHost(h) {
		return false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, x := range n.d[dir] {
		if x == h {
			return true
		}
	}
	if len(n.d[dir]) >= maxApprovedHosts {
		return false
	}
	n.d[dir] = append(n.d[dir], h)
	sort.Strings(n.d[dir])
	if n.path != "" {
		projects, rl := map[string][]string{}, map[string][]string{}
		for k, v := range n.d {
			if id, ok := strings.CutPrefix(k, "role:"); ok {
				rl[id] = v
			} else {
				projects[k] = v
			}
		}
		b, _ := json.Marshal(map[string]any{"projects": projects, "roles": rl})
		if err := vault.WriteFileAtomic(n.path, b, 0o600); err != nil {
			log.Printf("agent: saving approved hosts: %v", err)
		}
	}
	return true
}
