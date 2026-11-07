package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// Keys are the throwaway signing keys of one sandbox. Private halves stay in files and are not returned.
type Keys struct {
	Dir        string
	ReleasePub string // PEM
	PluginPub  string // base64 SPKI, what a plugin manifest carries
}

// InitKeys writes a release key and a plugin publisher key if they are not there yet.
func InitKeys(dir string) (Keys, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Keys{}, err
	}
	if err := ensureKey(filepath.Join(dir, "release.priv.pem"), filepath.Join(dir, "release.pub.pem")); err != nil {
		return Keys{}, err
	}
	if err := ensureKey(filepath.Join(dir, "plugin.priv.pem"), filepath.Join(dir, "plugin.pub.pem")); err != nil {
		return Keys{}, err
	}
	pub, err := os.ReadFile(filepath.Join(dir, "plugin.pub.pem"))
	if err != nil {
		return Keys{}, err
	}
	b64, err := pemPublicB64(pub)
	if err != nil {
		return Keys{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.pub.b64"), []byte(b64+"\n"), 0o600); err != nil {
		return Keys{}, err
	}
	rel, err := os.ReadFile(filepath.Join(dir, "release.pub.pem"))
	if err != nil {
		return Keys{}, err
	}
	return Keys{Dir: dir, ReleasePub: string(rel), PluginPub: b64}, nil
}

func ensureKey(privPath, pubPath string) error {
	if st, err := os.Stat(privPath); err == nil && st.Size() > 0 {
		return nil
	}
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}
	if err := os.WriteFile(privPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustMarshal(priv)}), 0o600); err != nil {
		return err
	}
	pub, err := x509.MarshalPKIXPublicKey(priv.Public())
	if err != nil {
		return err
	}
	return os.WriteFile(pubPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}), 0o644)
}

func mustMarshal(k ed25519.PrivateKey) []byte {
	b, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		panic(err)
	}
	return b
}

func pemPublicB64(pemBytes []byte) (string, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return "", fmt.Errorf("plugin public key is not PEM")
	}
	return base64.StdEncoding.EncodeToString(block.Bytes), nil
}

func loadPriv(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("%s is not PEM", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s is not Ed25519", path)
	}
	return priv, nil
}

// Publish packs pluginBin into a catalog signed with the sandbox release key.
// The plugin's own checksums are signed with the sandbox publisher key, which must already be the key in its manifest.
func Publish(keys Keys, pluginBin, root, sourceBase string) error {
	manifest, err := exec.Command(pluginBin, "--manifest").Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("manifest: %v\n%s", err, ee.Stderr)
		}
		return err
	}
	var m struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Version     string `json:"version"`
		Description string `json:"description"`
		MinRubi     string `json:"min_rubi"`
		Entry       string `json:"entry"`
		Publisher   struct {
			Name string `json:"name"`
			Key  string `json:"key"`
		} `json:"publisher"`
	}
	if err := json.Unmarshal(manifest, &m); err != nil || m.ID == "" || m.Entry == "" || m.Version == "" {
		return fmt.Errorf("plugin manifest is not usable")
	}
	if m.Publisher.Key != keys.PluginPub {
		return fmt.Errorf("plugin manifest key does not match the sandbox publisher key")
	}
	exe, err := os.ReadFile(pluginBin)
	if err != nil {
		return err
	}
	archive := tarGz(m.Entry, exe)
	name := fmt.Sprintf("%s-%s-%s.tar.gz", m.ID, runtime.GOOS, runtime.GOARCH)
	sums := fmt.Sprintf("%s  rubi-plugin.json\n%s  %s\n", sha(manifest), sha(archive), name)
	pluginPriv, err := loadPriv(filepath.Join(keys.Dir, "plugin.priv.pem"))
	if err != nil {
		return err
	}
	releasePriv, err := loadPriv(filepath.Join(keys.Dir, "release.priv.pem"))
	if err != nil {
		return err
	}
	sig := ed25519.Sign(pluginPriv, []byte(sums))
	verDir := filepath.Join(root, m.ID, m.Version)
	if err := os.MkdirAll(verDir, 0o755); err != nil {
		return err
	}
	files := map[string][]byte{
		"rubi-plugin.json": manifest,
		name:               archive,
		"SHA256SUMS":       []byte(sums),
		"SHA256SUMS.sig":   sig,
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(verDir, n), b, 0o644); err != nil {
			return err
		}
	}
	latest := filepath.Join(root, m.ID, "latest")
	_ = os.RemoveAll(latest)
	if err := os.MkdirAll(latest, 0o755); err != nil {
		return err
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(latest, n), b, 0o644); err != nil {
			return err
		}
	}
	source := stringsTrim(sourceBase) + "/" + m.ID
	cat, err := json.MarshalIndent(map[string]any{
		"schema": 1, "updated_at": time.Now().UTC().Format(time.RFC3339),
		"plugins": []any{map[string]any{
			"id": m.ID, "name": m.Name, "summary": m.Description,
			"publisher": map[string]string{"name": m.Publisher.Name, "key": m.Publisher.Key},
			"source":    source,
			"versions":  []any{map[string]string{"version": m.Version, "sums_sha256": sha([]byte(sums)), "min_rubi": m.MinRubi}},
		}},
	}, "", "  ")
	if err != nil {
		return err
	}
	cat = append(cat, '\n')
	mp := filepath.Join(root, "marketplace")
	if err := os.MkdirAll(mp, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(mp, "catalog.json"), cat, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(mp, "catalog.json.sig"), ed25519.Sign(releasePriv, cat), 0o644)
}

func stringsTrim(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func tarGz(name string, exe []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(exe)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(exe)
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}
