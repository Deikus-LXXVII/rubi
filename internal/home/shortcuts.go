package home

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Shortcuts: Rubi sees and runs only the shortcuts in one folder (Rubi, by default), which the user
// fills on purpose, e.g. "Heating on" made of Apple Home actions. A shortcut runs by its identifier,
// so one with the same name elsewhere can't be run instead.

const (
	shortcutTimeout = 90 * time.Second
	maxOutput       = 64 << 10
)

func runShortcuts(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "shortcuts", args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		if msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return out, nil
}

var listLine = regexp.MustCompile(`^(.*) \(([0-9A-Fa-f-]{36})\)$`)

type shortcut struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

func (h *Helper) shortcuts(ctx context.Context) ([]shortcut, error) {
	folder := h.Fresh().ShortcutsFolder
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := h.Shortcuts(ctx, "list", "--folder-name", folder, "--show-identifiers")
	if err != nil {
		return nil, fmt.Errorf("couldn't list the shortcuts in the %q folder (create it in the Shortcuts app): %w", folder, err)
	}
	list := []shortcut{}
	for _, line := range strings.Split(string(out), "\n") {
		if m := listLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			list = append(list, shortcut{Name: m[1], ID: m[2]})
		}
	}
	return list, nil
}

func (h *Helper) listShortcuts(ctx context.Context) (any, error) {
	list, err := h.shortcuts(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list))
	for _, s := range list {
		names = append(names, s.Name)
	}
	return map[string]any{"folder": h.Config().ShortcutsFolder, "shortcuts": names}, nil
}

func (h *Helper) runShortcut(ctx context.Context, name, input string) (any, error) {
	list, err := h.shortcuts(ctx)
	if err != nil {
		return nil, err
	}
	var target *shortcut
	for i := range list {
		if list[i].Name == name {
			target = &list[i]
		}
	}
	if target == nil {
		return nil, fmt.Errorf("there is no shortcut %q in the %q folder", name, h.Config().ShortcutsFolder)
	}
	if len(input) > maxOutput {
		return nil, errors.New("input too long")
	}
	tmp, err := os.MkdirTemp("", "rubi-home-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	args := []string{"run", target.ID, "--output-path", filepath.Join(tmp, "out.txt"), "--output-type", "public.plain-text"}
	if input != "" {
		in := filepath.Join(tmp, "in.txt")
		if err := os.WriteFile(in, []byte(input), 0o600); err != nil {
			return nil, err
		}
		args = append(args, "--input-path", in)
	}
	rctx, cancel := context.WithTimeout(ctx, shortcutTimeout)
	defer cancel()
	if _, err := h.Shortcuts(rctx, args...); err != nil {
		return nil, fmt.Errorf("the shortcut %q failed: %w", name, err)
	}
	var out []byte
	if f, err := os.Open(filepath.Join(tmp, "out.txt")); err == nil {
		out, _ = io.ReadAll(io.LimitReader(f, maxOutput+1)) // a runaway shortcut can't fill the memory
		f.Close()
	}
	text := string(out)
	truncated := len(text) > maxOutput
	if truncated {
		text = text[:maxOutput]
	}
	return map[string]any{"ran": name, "output": text, "truncated": truncated}, nil
}
