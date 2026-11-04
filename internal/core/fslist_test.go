package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// folderHome makes a home folder for the Rubi Home behind the fake: under the working directory, because a system
// temp folder is a system tree and the folder browser refuses those.
func folderHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", "fshome-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	abs, _ := filepath.Abs(dir)
	real, _ := filepath.EvalSymlinks(abs)
	t.Setenv("HOME", real)
	t.Setenv("USERPROFILE", real)
	return real
}

// The panel's folder browser asks a paired Rubi Home for the folders of a place; what comes back is folder names.
func TestListDeviceFoldersAsksHomeForFolderNamesOnly(t *testing.T) {
	home := folderHome(t)
	os.MkdirAll(filepath.Join(home, "projects", "app"), 0o755)
	os.WriteFile(filepath.Join(home, "diary.txt"), []byte("DEAR DIARY"), 0o600)
	c, _, _ := botCore(t)
	dev := pairedFakeHome(t, c, &fakeHook{})
	out, err := c.ListDeviceFolders(context.Background(), dev, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if out["path"] != home || !strings.Contains(string(raw), `"projects"`) {
		t.Fatalf("%s", raw)
	}
	for _, bad := range []string{"diary", "DEAR DIARY"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("the answer holds %q: %s", bad, raw)
		}
	}
	sub, err := c.ListDeviceFolders(context.Background(), dev, filepath.Join(home, "projects"))
	if err != nil || sub["parent"] != home || !strings.Contains(mustJSON(sub), `"app"`) {
		t.Fatalf("%v %v", sub, err)
	}
	// a place Home refuses comes back as an error, not as a listing
	if _, err := c.ListDeviceFolders(context.Background(), dev, string(filepath.Separator)); err == nil {
		t.Error("the root of the disk was listed")
	}
	if _, err := c.ListDeviceFolders(context.Background(), "nope", ""); err == nil {
		t.Error("an unknown computer was asked")
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// No plugin can list folders: not with the capability names that exist, and not with one named "fs" even if a
// manifest could declare it. The refusal comes before Home is asked.
func TestAPluginCannotListFolders(t *testing.T) {
	home := folderHome(t)
	os.MkdirAll(filepath.Join(home, "private-project-name"), 0o755)
	c, _, _ := botCore(t)
	f := &fakeHook{}
	dev := pairedFakeHome(t, c, f)
	for _, allowed := range [][]string{{"fs"}, {"hue", "shortcuts", "claude", "claude-sessions", "claude-start", "claude-agent", "agent-setup", "t3-agent", "t3-setup"}} {
		for _, op := range []string{"fs.list", "agent-setup.fs.list", "t3-setup.fs.list"} {
			out, err := c.homeCall(context.Background(), "some-plugin", allowed, dev, op, json.RawMessage(`{"path":""}`))
			if err == nil || strings.Contains(string(out), "private-project-name") {
				t.Errorf("%v %s: listed %s", allowed, op, out)
			}
		}
	}
	// the refusal is core's own, with its reason, for the one capability that would reach the op
	_, err := c.homeCall(context.Background(), "some-plugin", []string{"fs"}, dev, "fs.list", json.RawMessage(`{"path":""}`))
	if err == nil || !strings.Contains(err.Error(), "Rubi refused") {
		t.Fatalf("%v", err)
	}
	if _, err := c.attestHomeCall("some-plugin", "t3-setup.fs.list", nil); err != nil {
		t.Fatalf("only the op fs.list is the browser's: %v", err)
	}
}
