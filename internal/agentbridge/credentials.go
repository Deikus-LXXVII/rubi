package agentbridge

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
)

var credentialName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*(API_KEY|ACCESS_TOKEN|AUTH_TOKEN|SESSION_TOKEN)$`)

// Credentials reads only an explicitly provisioned owner-controlled file, after
// proving it is outside every sandbox write grant. Never returns file content in errors.
func Credentials(policy agentsandbox.Policy, path string) (map[string]string, error) {
	if path == "" {
		return map[string]string{}, nil
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("bridge: absolute credential path required")
	}
	settings, err := agentsandbox.Build(policy, nil)
	if err != nil {
		return nil, err
	}
	for _, w := range settings.Filesystem.AllowWrite {
		rel, e := filepath.Rel(w, filepath.Clean(path))
		if e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, errors.New("bridge: credential path is agent-writable")
		}
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, errors.New("bridge: credential file unresolved")
	}
	covered := false
	for _, d := range settings.Filesystem.DenyRead {
		rel, e := filepath.Rel(d, path)
		if e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			covered = true
		}
	}
	if !covered {
		return nil, errors.New("bridge: credential file must be hidden by policy")
	}
	for _, r := range settings.Filesystem.AllowRead {
		rel, e := filepath.Rel(r, path)
		if e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, errors.New("bridge: credential file covered by a runtime read exception")
		}
	}
	if _, err := agentsandbox.Build(policy, []string{path}); err != nil {
		return nil, errors.New("bridge: credential file overlaps sandbox or is unresolved")
	}
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 1<<20 {
		return nil, errors.New("bridge: credential file must be a private regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("bridge: cannot read provisioned credentials")
	}
	var values map[string]string
	if json.Unmarshal(data, &values) != nil || values == nil {
		return nil, errors.New("bridge: credentials must be a JSON environment map")
	}
	for key := range values {
		if strings.HasPrefix(key, "T3CODE_") || strings.HasPrefix(key, "RUBI_") {
			return nil, errors.New("bridge: reserved credential environment name")
		}
		if !credentialName.MatchString(key) && key != "AWS_ACCESS_KEY_ID" && key != "AWS_SECRET_ACCESS_KEY" {
			return nil, errors.New("bridge: credential environment contains an unsupported name")
		}
	}
	return values, nil
}
