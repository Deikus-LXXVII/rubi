package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The approval shows a path; the folder Home adds must be that very folder. Before a project.add with a folder goes to
// Home, core asks Home what it would add (project.resolve), and refuses when that is another path than the approved
// one, as a link or a path a listing passed on in another form would be.
func TestProjectAddIsRefusedUnlessHomeResolvesTheApprovedPathToItself(t *testing.T) {
	c, _, _ := botCore(t)
	f := &fakeHook{resolveTo: map[string]string{"/Users/alice/link": "/Users/alice/real"}}
	dev := pairedFakeHome(t, c, f)
	run := func(op, dir string) error {
		args := json.RawMessage(`{"dir":"` + dir + `"}`)
		payload := json.RawMessage(`{"device":"` + dev + `","device_name":"Mac","dir":"` + dir + `"}`)
		var err error
		decide(t, c, "claude-code", kindSetupFolder, payload, func(string) {
			_, err = c.homeCall(context.Background(), "claude-code", []string{"agent-setup", "t3-setup"}, dev, op, args)
		})
		return err
	}
	for _, op := range []string{"agent-setup.project.add", "t3-setup.project.add"} {
		f.adds = nil
		if err := run(op, "/Users/alice/real"); err != nil || len(f.adds) != 1 || f.adds[0] != "/Users/alice/real" {
			t.Fatalf("%s: the folder Home resolves to itself: %v, adds %v", op, err, f.adds)
		}
		f.adds = nil
		err := run(op, "/Users/alice/link")
		if err == nil || !strings.Contains(err.Error(), "Rubi refused") || !strings.Contains(err.Error(), "/Users/alice/real") {
			t.Fatalf("%s: a link: %v", op, err)
		}
		if len(f.adds) != 0 {
			t.Fatalf("%s: the add reached Home: %v", op, f.adds)
		}
	}
	// Home refusing to resolve (the folder is private, or a project already) refuses the add too
	f.resolveErr = errors.New("a system folder can't be a project")
	f.adds = nil
	if err := run("agent-setup.project.add", "/Users/alice/real"); err == nil || len(f.adds) != 0 {
		t.Fatalf("a folder Home will not resolve: %v, adds %v", err, f.adds)
	}
	f.resolveErr = nil
	// a project.add with no folder is Home's to refuse; core does not resolve nothing
	if _, err := c.homeCall(context.Background(), "claude-code", []string{"agent-setup"}, dev, "agent-setup.project.add", json.RawMessage(`{}`)); err != nil {
		t.Errorf("no folder: %v", err)
	}
}
