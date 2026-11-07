// Command t3agents is the sandbox stand-in for the T3 Agents plugin. It speaks the same panel page
// the real plugin does for first-time setup, and it drives the fake Home's t3-setup operations.
// A build of https://github.com/Deikus-LXXVII/rubi-t3agents replaces it when that build succeeds.
package main

import (
	"context"
	"encoding/json"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// PublisherKey is set at build time to the sandbox's throwaway publisher key.
var PublisherKey = ""

func main() {
	p := rubiplugin.New(rubiplugin.Manifest{
		ID: "t3agents", Name: "T3 Agents", Version: "v0.1.0",
		Description: "Sandbox stand-in for T3 Agents. First-time setup is driven against the fake Rubi Home.",
		Needs:       "A Rubi Home that advertises t3-setup.",
		Publisher:   rubiplugin.Publisher{Name: "Sandbox", Key: PublisherKey},
		Source:      "sandbox://t3agents",
		MinRubi:     "v0.14.0",
		Entry:       "t3agents",
		Panel:       "view",
		Home:        []string{"t3-setup"},
	})
	p.OnPanel = onPanel
	p.Main()
}

func onPanel(ctx context.Context, h *rubiplugin.Host, op string, args json.RawMessage) (any, error) {
	var in struct {
		Page    string `json:"page"`
		Device  string `json:"device"`
		Step    string `json:"step"`
		Method  string `json:"method"`
		Project string `json:"project"`
		Code    string `json:"code"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &in)
	}
	dev, err := oneDevice(h, in.Device)
	if err != nil {
		return nil, err
	}
	switch op {
	case "view":
		if in.Page == "setup" {
			return setupPage(ctx, h, dev)
		}
		return mainPage(dev), nil
	case "setup_run":
		body := map[string]any{"step": in.Step}
		if in.Method != "" {
			body["method"] = in.Method
		}
		if in.Project != "" {
			body["role"] = in.Project
		}
		return call(ctx, h, dev, "t3-setup.run", body)
	case "setup_open":
		return call(ctx, h, dev, "t3-setup.login.open", map[string]any{})
	case "setup_code":
		return call(ctx, h, dev, "t3-setup.login.code", map[string]any{"code": in.Code})
	case "setup_cancel":
		return call(ctx, h, dev, "t3-setup.cancel", map[string]any{})
	default:
		return nil, errString("unknown op")
	}
}

type device struct{ ID, Name string }

func oneDevice(h *rubiplugin.Host, want string) (device, error) {
	list, err := h.HomeDevices()
	if err != nil {
		return device{}, err
	}
	if len(list) == 0 {
		return device{}, errString("no Rubi Home is paired yet")
	}
	if want == "" && len(list) == 1 {
		return device{list[0].ID, list[0].Name}, nil
	}
	for _, d := range list {
		if d.ID == want || d.Name == want {
			return device{d.ID, d.Name}, nil
		}
	}
	return device{}, errString("that computer doesn't report the t3-setup capability")
}

func mainPage(d device) map[string]any {
	return map[string]any{
		"title": "T3 Agents",
		"note":  "Sandbox stand-in. Open setup to walk container, image, T3, pair and sign-in.",
		"sections": []any{map[string]any{
			"title": "First-time setup",
			"items": []any{map[string]any{
				"title":   "Set up T3 Agents",
				"actions": []any{map[string]any{"label": "Open", "open": map[string]any{"page": "setup", "device": d.ID}}},
			}},
		}},
	}
}

func setupPage(ctx context.Context, h *rubiplugin.Host, d device) (any, error) {
	var st struct {
		Steps []struct {
			ID, Title, State, Reason, Action string
			Projects                         []struct {
				ID, Dir, State, Reason, Action string
			}
		}
		Login *struct {
			URL       string `json:"url"`
			WantsCode bool   `json:"wants_code"`
			Role      string `json:"role"`
		}
	}
	if err := h.HomeCall(ctx, d.ID, "t3-setup.status", map[string]any{}, &st); err != nil {
		return nil, err
	}
	dev := map[string]any{"device": d.ID}
	var sections []any
	n := 0
	for _, s := range st.Steps {
		if s.ID == "projects" {
			continue
		}
		n++
		head := map[string]any{"title": s.Title + ": " + s.State, "detail": s.Reason}
		var actions []any
		add := func(label, step, method, project string) {
			args := map[string]any{"device": d.ID, "step": step}
			if method != "" {
				args["method"] = method
			}
			if project != "" {
				args["project"] = project
			}
			actions = append(actions, map[string]any{"label": label, "op": "setup_run", "args": args})
		}
		switch s.Action {
		case "start":
			add("Start", "service", "", "")
		case "build":
			add("Build", "image", "", "")
		case "install":
			add("Install the pinned T3", "t3", "", "")
		case "pair":
			add("Pair", "pair", "", "")
		case "selftest":
			add("Run self-test", "selftest", "", "")
		case "cancel":
			actions = append(actions, map[string]any{"label": "Cancel", "op": "setup_cancel", "args": dev})
		}
		head["actions"] = actions
		items := []any{head}
		for _, pr := range s.Projects {
			it := map[string]any{"title": pr.Dir, "detail": pr.State + ". " + pr.Reason}
			var pa []any
			if pr.Action == "login" || pr.Action == "login_again" {
				label := "Sign in"
				if pr.Action == "login_again" {
					label = "Sign in again"
				}
				pa = append(pa, map[string]any{"label": label, "op": "setup_run", "args": map[string]any{"device": d.ID, "step": "login", "method": "claudeai", "project": pr.ID}})
			}
			it["actions"] = pa
			items = append(items, it)
		}
		if s.ID == "login" && st.Login != nil && st.Login.URL != "" {
			items = append(items, map[string]any{
				"title":  "Sign-in waiting for the browser",
				"detail": "The full link opens from the button. It is not opened on the home computer.",
				"actions": []any{
					map[string]any{"label": "Open the sign-in page", "op": "setup_open", "args": dev},
					map[string]any{"label": "Paste the code", "op": "setup_code", "args": dev,
						"prompt": map[string]any{"arg": "code", "label": "Paste the code shown on the Claude page", "value": ""}},
				},
			})
		}
		sections = append(sections, map[string]any{"title": s.Title, "items": items})
	}
	return map[string]any{
		"title": "Set up T3 Agents: " + d.Name, "up": map[string]any{"label": "T3 Agents", "open": map[string]any{}},
		"note": "Sandbox stand-in for the T3 Agents setup page.", "sections": sections, "refresh": 3,
	}, nil
}

func call(ctx context.Context, h *rubiplugin.Host, d device, op string, args map[string]any) (map[string]any, error) {
	var res map[string]any
	if err := h.HomeCall(ctx, d.ID, op, args, &res); err != nil {
		return nil, err
	}
	if res == nil {
		res = map[string]any{"ok": true}
	}
	return res, nil
}

type strErr string

func (e strErr) Error() string { return string(e) }
func errString(s string) error { return strErr(s) }
