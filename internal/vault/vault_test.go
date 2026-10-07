package vault

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	dek := NewKey()
	d := NewData("inst-1")
	d.Version = 3
	d.Integrations["icloud-mail"] = &Integration{Enabled: true, Secrets: map[string]string{"app_password": "s3cret"}}

	blob, err := Seal(dek, d)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("s3cret")) {
		t.Fatal("secret visible in sealed vault")
	}
	got, err := Open(dek, "inst-1", blob)
	if err != nil {
		t.Fatal(err)
	}
	if got.Integrations["icloud-mail"].Secrets["app_password"] != "s3cret" || got.Version != 3 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestOpenRejectsWrongKeyAndInstance(t *testing.T) {
	dek := NewKey()
	blob, _ := Seal(dek, NewData("inst-1"))
	if _, err := Open(NewKey(), "inst-1", blob); !errors.Is(err, ErrBadKey) {
		t.Fatalf("wrong key: got %v", err)
	}
	if _, err := Open(dek, "inst-2", blob); !errors.Is(err, ErrBadKey) {
		t.Fatalf("wrong instance: got %v", err)
	}
}

func TestTamperedHeaderDetected(t *testing.T) {
	dek := NewKey()
	d := NewData("inst-1")
	d.Version = 5
	blob, _ := Seal(dek, d)

	var env map[string]any
	_ = json.Unmarshal(blob, &env)
	env["version"] = 9 // try to pass an old vault off as newer
	tampered, _ := json.Marshal(env)
	if _, err := Open(dek, "inst-1", tampered); err == nil {
		t.Fatal("tampered header accepted")
	}
}

func TestStoreLifecycleAndRollback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vault.sealed")
	s := NewStore(path, "inst-1")
	dek := NewKey()

	if err := s.Create(dek, NewData("")); err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(path) // version 1

	if err := s.Update(func(d *Data) error { d.Policy["icloud-mail.send"] = "strong"; return nil }); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Version(); v != 2 {
		t.Fatalf("version = %d, want 2", v)
	}

	s.Lock()
	if err := s.View(func(*Data) error { return nil }); !errors.Is(err, ErrLocked) {
		t.Fatalf("view while locked: %v", err)
	}
	if err := s.Unlock(NewKey(), 0); !errors.Is(err, ErrBadKey) {
		t.Fatalf("unlock with wrong key: %v", err)
	}
	if err := s.Unlock(dek, 2); err != nil {
		t.Fatal(err)
	}
	_ = s.View(func(d *Data) error {
		if d.Policy["icloud-mail.send"] != "strong" {
			t.Fatal("update lost")
		}
		return nil
	})

	// Roll the file back to version 1: the panel's minimum version must catch it.
	s.Lock()
	_ = os.WriteFile(path, old, 0o600)
	if err := s.Unlock(dek, 2); !errors.Is(err, ErrVersion) {
		t.Fatalf("rollback not detected: %v", err)
	}
}

func TestFailedUpdateLeavesDataUnchanged(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "v"), "i")
	_ = s.Create(NewKey(), NewData(""))
	_ = s.Update(func(d *Data) error { d.Locale = "ru"; return errors.New("boom") })
	_ = s.View(func(d *Data) error {
		if d.Locale != "" {
			t.Fatal("failed update leaked into data")
		}
		return nil
	})
}

func TestSaveKeysRefusesEmpty(t *testing.T) {
	if err := SaveKeys(filepath.Join(t.TempDir(), "k"), &Keys{Instance: "i"}); err == nil {
		t.Fatal("saved keys without wraps")
	}
}
