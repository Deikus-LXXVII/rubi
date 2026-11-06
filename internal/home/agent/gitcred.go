package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// The git credential helper. The helper in the VM (git-credential-rubi) forwards `git credential get`,
// `store` and `erase` through the runner as a git.credential request. Reading is always served from a
// configured source; a credential for a push is given only after the plugin approved it (a cred.ask item
// answered with claude-agent.decide; the mandate check lives in the plugin). Reads and pushes use
// separate credentials, so a read-only token can't push even if a request is mislabelled. store and erase
// are accepted and ignored: nothing the VM says is kept on the Mac.

// Cred is a username and token for one host.
type Cred struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// CredSource gives credentials. ok is false when there is none for the host.
type CredSource interface {
	Read(host string) (c Cred, ok bool)
	Push(host string) (c Cred, ok bool)
}

// FileCredSource reads a JSON file (0600) of {"git.example.com": {"read": {...}, "push": {...}}}.
type FileCredSource struct{ Path string }

type credFileEntry struct {
	Read *Cred `json:"read"`
	Push *Cred `json:"push"`
}

func (f FileCredSource) load(host string) (credFileEntry, bool) {
	st, err := os.Stat(f.Path)
	if err != nil || st.Mode().Perm()&0o077 != 0 || st.Size() > 1<<20 {
		return credFileEntry{}, false // a file others can read isn't used
	}
	file, err := os.Open(f.Path)
	if err != nil {
		return credFileEntry{}, false
	}
	defer file.Close()
	b, _ := io.ReadAll(io.LimitReader(file, 1<<20))
	var m map[string]credFileEntry
	if json.Unmarshal(b, &m) != nil {
		return credFileEntry{}, false
	}
	e, ok := m[host]
	return e, ok
}

func (f FileCredSource) Read(host string) (Cred, bool) {
	if e, ok := f.load(host); ok && e.Read != nil {
		return *e.Read, true
	}
	return Cred{}, false
}

func (f FileCredSource) Push(host string) (Cred, bool) {
	if e, ok := f.load(host); ok && e.Push != nil {
		return *e.Push, true
	}
	return Cred{}, false
}

// parseCredInput reads git's key=value lines (up to the blank line).
func parseCredInput(s string) (map[string]string, error) {
	if len(s) > 4096 {
		return nil, errors.New("credential request too long")
	}
	kv := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" {
			return nil, errors.New("bad credential request")
		}
		kv[k] = v
	}
	return kv, nil
}

func credOutput(host string, c Cred) string {
	return fmt.Sprintf("protocol=https\nhost=%s\nusername=%s\npassword=%s\n\n", host, c.Username, c.Password)
}
