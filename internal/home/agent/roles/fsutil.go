package roles

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

func tmpName(dir, prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return filepath.Join(dir, prefix+hex.EncodeToString(b[:]))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// writeTemp writes data to a new file next to path (0 = path's directory) and syncs it. The caller renames or
// links it into place.
func writeTemp(dir string, data []byte, perm os.FileMode) (string, error) {
	tmp := tmpName(dir, ".tmp-")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// writeAtomic replaces path with data (temp file, sync, rename, sync the directory).
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := writeTemp(dir, data, perm)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

var errExists = errors.New("already exists")

// writeNew puts data at path only if nothing is there: it never replaces a file.
func writeNew(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := writeTemp(dir, data, perm)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, path); err != nil {
		if os.IsExist(err) {
			return errExists
		}
		if _, serr := os.Lstat(path); serr == nil {
			return errExists
		}
		// No hard links here: fall back to a check and a rename (the role's lock keeps writers apart).
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	return syncDir(dir)
}

// swapLink points the symlink at link to target (relative), atomically: a new symlink is renamed over the old.
func swapLink(link, target string) error {
	dir := filepath.Dir(link)
	tmp := tmpName(dir, ".tmp-link-")
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}
