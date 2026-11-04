package hostpaths

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsPaths(t *testing.T) {
	base, err := DataBase()
	if err != nil || !strings.EqualFold(filepath.Base(filepath.Dir(base)), "AppData") || !strings.EqualFold(filepath.Base(base), "Local") {
		t.Errorf("DataBase %q %v: want %%LOCALAPPDATA%%, not the roaming folder", base, err)
	}
	if h := HelperHome(`C:\Users\a`); !strings.HasSuffix(h, `\Rubi Home`) {
		t.Errorf("HelperHome %q", h)
	}
	if h := HookBinary(); !strings.EqualFold(filepath.Base(h), "rubi-home.exe") || !strings.Contains(strings.ToLower(h), "program files") {
		t.Errorf("HookBinary %q", h)
	}
	if !strings.HasSuffix(ManagedSettings(), `ClaudeCode\managed-settings.json`) {
		t.Errorf("ManagedSettings %q", ManagedSettings())
	}
	win := false
	for _, d := range SensitiveSysDirs() {
		win = win || strings.EqualFold(filepath.Base(d), "Windows")
	}
	if !win {
		t.Errorf("the Windows folder is not denied: %v", SensitiveSysDirs())
	}
}
