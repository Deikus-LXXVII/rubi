package home

import "testing"

func TestLoginStepNamesTheEarlierStepsItIsWaitingOn(t *testing.T) {
	step := map[string]any{
		"id": "login", "state": "todo", "reason": "No credential file yet.", "action": "login",
		"projects": []map[string]any{{"id": "reviewer", "state": "todo", "action": "login", "reason": "Not signed in"}},
	}
	before := []any{
		map[string]any{"id": "container", "title": "Apple container", "state": "done"},
		map[string]any{"id": "service", "title": "Container service", "state": "todo"},
		map[string]any{"id": "network", "title": "Network", "state": "done"},
		map[string]any{"id": "t3", "title": "T3", "state": "todo"},
	}
	noteLoginWaiting(step, before)
	reason, _ := step["reason"].(string)
	if reason != "Waiting for earlier steps: Container service, T3." || step["action"] != "" {
		t.Fatalf("step: %#v", step)
	}
	rows := step["projects"].([]map[string]any)
	if rows[0]["action"] != "" || rows[0]["reason"] != reason {
		t.Fatalf("role row still offers sign-in: %#v", rows[0])
	}

	done := map[string]any{"id": "login", "state": "done", "reason": "A credential file is present.", "action": "login_again"}
	noteLoginWaiting(done, before)
	if done["reason"] != "A credential file is present." || done["action"] != "login_again" {
		t.Fatalf("a finished sign-in was rewritten: %#v", done)
	}

	ready := map[string]any{"id": "login", "state": "todo", "reason": "No credential file yet.", "action": "login"}
	noteLoginWaiting(ready, []any{map[string]any{"id": "container", "title": "Apple container", "state": "done"}})
	if ready["reason"] != "No credential file yet." || ready["action"] != "login" {
		t.Fatalf("sign-in hidden after the earlier steps were done: %#v", ready)
	}
}
