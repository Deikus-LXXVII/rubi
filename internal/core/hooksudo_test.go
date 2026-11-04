package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
)

const planHash = "f00dfeedf00dfeedf00dfeedf00dfeedf00dfeedf00dfeedf00dfeedf00dfeed"

// The approval shows the hash of the plan and what the person will be asked; the install call is told the hash.
func TestHookApprovalShowsThePlanHashAndTheInstallIsToldIt(t *testing.T) {
	c, _, _ := botCore(t)
	f := &fakeHook{planHash: planHash, admin: "Your desktop will ask for the administrator password on that computer (polkit); approve only if you asked for this"}
	dev := pairedFakeHome(t, c, f)
	ctx := context.Background()
	id, err := c.InstallHookOnDevice(ctx, dev)
	if err != nil {
		t.Fatal(err)
	}
	det, _ := c.Approvals.Detail(id)
	pv, _ := det.Preview.(map[string]any)
	if pv["plan"] != planHash || pv["check"] != f.admin || strings.Contains(pv["check"].(string), "macOS") {
		t.Fatalf("preview %v", pv)
	}
	snap, err := c.Approvals.DecidePanel(ctx, id, "apply", true, approvals.Verified)
	if err != nil || snap.State != approvals.Executed {
		t.Fatalf("%+v %v", snap, err)
	}
	var args map[string]any
	raw, _ := f.installArgs.Load().(json.RawMessage)
	if err := json.Unmarshal(raw, &args); err != nil || args["plan_sha256"] != planHash {
		t.Fatalf("hook.install was called with %s", raw)
	}
	if m, _ := snap.Result.(map[string]any); !strings.Contains(m["receipt"].(string), "administrator prompt is open") {
		t.Fatalf("receipt %v", snap.Result)
	}
}

// No one can be asked for the password there: the result carries the command for the Bot, only in the one shape, only if the
// script in it has the hash the person approved. Nothing else is passed on, whatever it looks like.
func TestHookNeedsBotSudoResult(t *testing.T) {
	script := "set -e; umask 022\nsrc='/home/ann/.rubi-home/bin/rubi-home'; echo install\n"
	sum := sha256.Sum256([]byte(script))
	hash := hex.EncodeToString(sum[:])
	good := []string{"/usr/bin/sudo", "--", "/bin/sh", "-c", script}
	run := func(argv []string, approved string) (any, error) {
		c, _, _ := botCore(t)
		f := &fakeHook{planHash: approved, sudoArgv: argv}
		dev := pairedFakeHome(t, c, f)
		id, err := c.InstallHookOnDevice(context.Background(), dev)
		if err != nil {
			t.Fatal(err)
		}
		snap, err := c.Approvals.DecidePanel(context.Background(), id, "apply", true, approvals.Verified)
		if err != nil {
			t.Fatal(err)
		}
		return snap.Result, nil
	}
	res, _ := run(good, hash)
	m, _ := res.(map[string]any)
	got, _ := m["argv"].([]string)
	if m["needs_bot_sudo"] != true || m["plan_sha256"] != hash || !reflect.DeepEqual(got, good) || m["command"] != nil || !strings.Contains(m["receipt"].(string), hash) {
		t.Fatalf("result %v", res)
	}
	long := strings.Repeat("x", 600<<10)
	longSum := sha256.Sum256([]byte(long))
	for name, c := range map[string]struct {
		argv     []string
		approved string
	}{
		"a script with another hash":        {[]string{"/usr/bin/sudo", "--", "/bin/sh", "-c", script + "rm -rf /\n"}, hash},
		"another hash approved":             {good, strings.Repeat("0", 64)},
		"a trailing argument":               {append(append([]string{}, good...), "--evil"), hash},
		"a trailing flag before the script": {[]string{"/usr/bin/sudo", "--", "/bin/sh", "-c", script, "-c", "id"}, hash},
		"a duplicate flag":                  {[]string{"/usr/bin/sudo", "--", "/bin/sh", "-c", "-c", script}, hash},
		"another program":                   {[]string{"/usr/bin/sudo", "--", "/bin/bash", "-c", script}, hash},
		"sudo by name":                      {[]string{"sudo", "--", "/bin/sh", "-c", script}, hash},
		"sudo from /tmp":                    {[]string{"/tmp/sudo", "--", "/bin/sh", "-c", script}, hash},
		"no double dash":                    {[]string{"/usr/bin/sudo", "-u", "root", "/bin/sh", "-c", script}, hash},
		"sh -s":                             {[]string{"/usr/bin/sudo", "--", "/bin/sh", "-s", script}, hash},
		"Rubi Home's binary":                {[]string{"/usr/bin/sudo", "--", "/home/ann/.rubi-home/bin/rubi-home", "install-hook", "--expect-plan-sha256", hash}, hash},
		"too short":                         {[]string{"/usr/bin/sudo", "--", "/bin/sh", "-c"}, hash},
		"an empty script":                   {[]string{"/usr/bin/sudo", "--", "/bin/sh", "-c", ""}, hash},
		"a script over the limit":           {[]string{"/usr/bin/sudo", "--", "/bin/sh", "-c", long}, hex.EncodeToString(longSum[:])},
		"no hash approved":                  {good, ""},
	} {
		res, _ := run(c.argv, c.approved)
		if m, _ := res.(map[string]any); m["needs_bot_sudo"] == true || m["argv"] != nil {
			t.Errorf("%s: passed on %v", name, res)
		}
	}
}
