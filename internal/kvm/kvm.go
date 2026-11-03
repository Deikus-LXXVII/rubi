// Package kvm makes /dev/kvm usable by the user Rubi runs as, for the microVM execution backend on a Linux server
// host. Some hosts give /dev/kvm to a group the user is not in, and an ACL set by hand is lost on every reboot or
// system update. With the opt-in setting, Rubi re-applies a user ACL through passwordless sudo at every start;
// without it, or without sudo, it refuses to start the backend and says why. Nothing here names a user or group:
// both are read from the system.
package kvm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"syscall"
	"time"
)

// Device is the KVM device.
const Device = "/dev/kvm"

// Options configures Ensure.
type Options struct {
	// GrantWithSudo is the install setting: re-apply a user ACL on the device through passwordless sudo when the
	// device isn't usable. Off by default.
	GrantWithSudo bool
	Device        string               // default Device
	Logf          func(string, ...any) // where the grant is logged
	usable        func(string) error   // seam: can this process open the device read-write?
	run           func(ctx context.Context, name string, args ...string) error
	username      func() (string, error) // seam: the user Rubi runs as
	owner         func(string) string    // seam: the device's group, for messages
}

// Ensure returns nil once the device can be opened read-write by this process. When it can't and GrantWithSudo is
// set, it runs `sudo -n setfacl -m u:<this user>:rw <device>` once and checks again. Every refusal says what to do.
func Ensure(ctx context.Context, o Options) error {
	o.defaults()
	dev := o.Device
	if _, err := os.Stat(dev); err != nil {
		return fmt.Errorf("kvm: %s is missing: this host can't run microVMs (%v)", dev, err)
	}
	if err := o.usable(dev); err == nil {
		return nil
	}
	name, err := o.username()
	if err != nil {
		return fmt.Errorf("kvm: can't tell which user Rubi runs as: %w", err)
	}
	if !validUser(name) {
		return fmt.Errorf("kvm: the user name %q can't be used in an ACL", name)
	}
	if !o.GrantWithSudo {
		return fmt.Errorf("kvm: %s belongs to group %q and %s can't open it; turn on the setting that lets Rubi grant access with sudo at start, or give %s access yourself", dev, o.owner(dev), name, name)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := o.run(ctx, "sudo", "-n", "setfacl", "-m", "u:"+name+":rw", dev); err != nil {
		return fmt.Errorf("kvm: granting %s access to %s with sudo failed (passwordless sudo and setfacl are needed): %w", name, dev, err)
	}
	o.Logf("kvm: granted %s read-write access to %s (user ACL through sudo; lost on reboot, re-applied at the next start)", name, dev)
	if err := o.usable(dev); err != nil {
		return fmt.Errorf("kvm: %s is still not usable after the grant: %w", dev, err)
	}
	return nil
}

func (o *Options) defaults() {
	if o.Device == "" {
		o.Device = Device
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.usable == nil {
		o.usable = func(p string) error {
			f, err := os.OpenFile(p, os.O_RDWR, 0)
			if err != nil {
				return err
			}
			return f.Close()
		}
	}
	if o.run == nil {
		o.run = func(ctx context.Context, name string, args ...string) error {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			if err != nil && len(out) > 0 {
				return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			}
			return err
		}
	}
	if o.username == nil {
		o.username = func() (string, error) {
			u, err := user.Current()
			if err != nil {
				return "", err
			}
			return u.Username, nil
		}
	}
	if o.owner == nil {
		o.owner = groupOf
	}
}

// validUser accepts the usual POSIX user names; anything else would change the meaning of the ACL entry.
func validUser(s string) bool {
	if s == "" || len(s) > 64 || s[0] == '-' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

func groupOf(p string) string {
	fi, err := os.Stat(p)
	if err != nil {
		return "?"
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "?"
	}
	g, err := user.LookupGroupId(fmt.Sprint(st.Gid))
	if err != nil {
		return fmt.Sprint(st.Gid)
	}
	return g.Name
}
