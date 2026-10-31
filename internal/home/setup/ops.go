package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Handle serves one agent-setup.* operation (see docs/design/home-setup-wizard.md).
func (s *Setup) Handle(ctx context.Context, op string, args json.RawMessage) (any, error) {
	var in struct {
		Step    string `json:"step"`
		Project string `json:"project"`
		Method  string `json:"method"`
		Option  string `json:"option"`
		Code    string `json:"code"`
		Dir     string `json:"dir"`
	}
	if len(args) > 0 && string(args) != "null" && json.Unmarshal(args, &in) != nil {
		return nil, errors.New("bad arguments")
	}
	switch strings.TrimPrefix(op, "agent-setup.") {
	case "status":
		return s.Status(ctx), nil
	case "run":
		if err := s.Run(ctx, in.Step, in.Project, in.Method, in.Option); err != nil {
			return nil, err
		}
		return map[string]any{"started": true}, nil
	case "login.code":
		if err := s.LoginCode(in.Project, in.Code); err != nil {
			return nil, err
		}
		return map[string]any{"sent": true}, nil
	case "login.open":
		if err := s.LoginOpen(ctx, in.Project); err != nil {
			return nil, err
		}
		return map[string]any{"opened": true}, nil
	case "project.add":
		dir, err := s.AddProject(ctx)
		if err != nil {
			return nil, err
		}
		if dir == "" {
			return map[string]any{"cancelled": true}, nil
		}
		return map[string]any{"added": dir}, nil
	case "project.remove":
		if err := s.RemoveProject(in.Dir); err != nil {
			return nil, err
		}
		return map[string]any{"removed": true}, nil
	case "cancel":
		return map[string]any{"stopped": s.Cancel()}, nil
	}
	return nil, fmt.Errorf("Rubi Home doesn't do %q", op)
}
