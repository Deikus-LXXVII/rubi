// Package plugins finds, verifies, installs and runs Rubi plugins (see docs/design/plugins.md).
package plugins

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/Deikus-LXXVII/rubi/internal/update"
	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

type Manifest = rubiplugin.Manifest

var (
	idPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{2,31}$`)
	namePattern = regexp.MustCompile(`^[a-z0-9_.-]{1,64}$`)
	toolPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// Check enforces the manifest rules: valid id, everything in the plugin's own namespace, a usable key.
func Check(m *Manifest) error {
	if m.Schema != 1 {
		return fmt.Errorf("unsupported manifest schema %d", m.Schema)
	}
	if m.API != rubiplugin.API {
		return fmt.Errorf("plugin uses API %d; this Rubi supports API %d", m.API, rubiplugin.API)
	}
	if !idPattern.MatchString(m.ID) || strings.HasSuffix(m.ID, "-") {
		return fmt.Errorf("invalid plugin id %q", m.ID)
	}
	if m.ID == "rubi" || strings.HasPrefix(m.ID, "rubi-") {
		return fmt.Errorf("plugin id %q is reserved", m.ID)
	}
	if strings.TrimSpace(m.Name) == "" || len(m.Name) > 64 {
		return errors.New("plugin name is missing or too long")
	}
	if !update.ValidVersion(m.Version) {
		return fmt.Errorf("invalid plugin version %q", m.Version)
	}
	if m.MinRubi != "" && !update.ValidVersion(m.MinRubi) {
		return fmt.Errorf("invalid min_rubi %q", m.MinRubi)
	}
	if _, err := PublicKey(m.Publisher.Key); err != nil {
		return fmt.Errorf("publisher key: %w", err)
	}
	if m.Entry == "" || path.IsAbs(m.Entry) || strings.Contains(m.Entry, "..") || !namePattern.MatchString(path.Base(m.Entry)) {
		return fmt.Errorf("invalid entry %q", m.Entry)
	}
	seen := map[string]bool{}
	unique := func(what, s string) error {
		if seen[what+s] {
			return fmt.Errorf("duplicate %s %q", what, s)
		}
		seen[what+s] = true
		return nil
	}
	for _, f := range m.Fields {
		if !namePattern.MatchString(f.Key) || f.Label == "" {
			return fmt.Errorf("invalid field %q", f.Key)
		}
		if err := unique("field", f.Key); err != nil {
			return err
		}
	}
	for _, s := range m.Secrets {
		if !namePattern.MatchString(s.Key) || s.Label == "" {
			return fmt.Errorf("invalid secret %q", s.Key)
		}
		if err := unique("field", s.Key); err != nil {
			return err
		}
	}
	for _, a := range m.Actions {
		if !strings.HasPrefix(a.Kind, m.ID+".") || !namePattern.MatchString(strings.TrimPrefix(a.Kind, m.ID+".")) {
			return fmt.Errorf("action kind %q must start with %q", a.Kind, m.ID+".")
		}
		switch a.DefaultLevel {
		case rubiplugin.None, rubiplugin.Chat, rubiplugin.Strong:
		default:
			return fmt.Errorf("action %s: invalid default level %q", a.Kind, a.DefaultLevel)
		}
		if a.Title == "" {
			return fmt.Errorf("action %s has no title", a.Kind)
		}
		if err := unique("action", a.Kind); err != nil {
			return err
		}
	}
	for _, e := range m.Events {
		if !namePattern.MatchString(e.Type) {
			return fmt.Errorf("invalid event type %q", e.Type)
		}
	}
	for _, f := range m.Config {
		if !namePattern.MatchString(f.Key) || f.Label == "" {
			return fmt.Errorf("invalid config field %q", f.Key)
		}
		if err := unique("config", f.Key); err != nil {
			return err
		}
		if err := CheckConfigValue(f, f.Default); f.Default != nil && err != nil {
			return fmt.Errorf("config %s default: %w", f.Key, err)
		}
	}
	prefix := rubiplugin.ToolPrefix(m.ID)
	for _, t := range m.Tools {
		if !strings.HasPrefix(t.Name, prefix) || !toolPattern.MatchString(t.Name) {
			return fmt.Errorf("tool %q must start with %q", t.Name, prefix)
		}
		if err := unique("tool", t.Name); err != nil {
			return err
		}
		var schema struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(t.InputSchema, &schema) != nil || schema.Type != "object" {
			return fmt.Errorf("tool %s: input_schema must be a JSON Schema object", t.Name)
		}
	}
	return nil
}

// CheckConfigValue validates a value for a config field (after JSON decoding).
func CheckConfigValue(f rubiplugin.ConfigField, v any) error {
	switch f.Type {
	case "bool":
		if _, ok := v.(bool); !ok {
			return errors.New("must be true or false")
		}
	case "text":
		s, ok := v.(string)
		if !ok || len(s) > 2000 {
			return errors.New("must be text up to 2000 characters")
		}
	case "list":
		list, ok := v.([]any)
		if !ok || len(list) > 500 {
			return errors.New("must be a list of up to 500 entries")
		}
		for _, x := range list {
			if s, ok := x.(string); !ok || len(s) > 200 {
				return errors.New("entries must be text up to 200 characters")
			}
		}
	default:
		return fmt.Errorf("unknown config type %q", f.Type)
	}
	return nil
}

// PublicKey decodes a publisher key (base64 SubjectPublicKeyInfo DER of an Ed25519 key).
func PublicKey(b64 string) (ed25519.PublicKey, error) {
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, errors.New("not base64")
	}
	k, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("not an Ed25519 key")
	}
	return pub, nil
}

// Fingerprint is a short, human-comparable form of a publisher key.
func Fingerprint(b64 string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(b64)))
	h := strings.ToUpper(hex.EncodeToString(sum[:8]))
	return h[0:4] + " " + h[4:8] + " " + h[8:12] + " " + h[12:16]
}

func verifySig(keyB64 string, msg, sig []byte) error {
	pub, err := PublicKey(keyB64)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, msg, sig) {
		return errors.New("bad signature")
	}
	return nil
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
