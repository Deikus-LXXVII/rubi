package agentbridge

import (
	"errors"
	"os"
	"path/filepath"
)

// StateEnvironment keeps cache/config/data distinct. Aliasing all XDG roots
// makes some providers invalidate their own persistent database as a cache.
func StateEnvironment(base string) (map[string]string, error) {
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, errors.New("bridge: state root unavailable")
	}
	defer root.Close()
	env := map[string]string{}
	for key, name := range map[string]string{"HOME": "home", "XDG_CONFIG_HOME": "config", "XDG_DATA_HOME": "data", "XDG_CACHE_HOME": "cache", "XDG_STATE_HOME": "state"} {
		path := filepath.Join(base, name)
		if err := root.MkdirAll(name, 0700); err != nil {
			return nil, errors.New("bridge: cannot provision private state")
		}
		st, err := root.Lstat(name)
		if err != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
			return nil, errors.New("bridge: private state must not contain public directories or symlinks")
		}
		env[key] = path
	}
	return env, nil
}
