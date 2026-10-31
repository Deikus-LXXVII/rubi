package setup

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// serviceWait is how long the service gets to come up.
var serviceWait = 60 * time.Second

func (s *Setup) runService(ctx context.Context, j *job) outcome {
	log := j.log(s)
	if st := s.probeService(ctx); st.State == Done {
		return outcome{Done, st.Reason}
	}
	log("Starting the container service...")
	if err := s.opt.Cmd.Stream(ctx, []string{"container", "system", "start", "--enable-kernel-install"}, log); err != nil {
		if ctx.Err() != nil {
			return outcome{Todo, "Cancelled."}
		}
		log(err.Error())
	}
	deadline := time.Now().Add(serviceWait)
	for {
		if st := s.probeService(ctx); st.State == Done {
			return outcome{Done, st.Reason}
		}
		if ctx.Err() != nil {
			return outcome{Todo, "Cancelled."}
		}
		if time.Now().After(deadline) {
			_, raw, _ := s.systemStatus(ctx)
			return outcome{Failed, "The service didn't start in " + serviceWait.String() + ". " + safeText(string(raw))}
		}
		select {
		case <-ctx.Done():
		case <-time.After(min(2*time.Second, serviceWait/4)):
		}
	}
}

// msgNetworkApproval: creating the network has its own approval, which "use the automatic address" doesn't give.
const msgNetworkApproval = "Creating the network needs its own approval."

// runNetwork creates the host-only network when it is missing. With autoAddress it never creates anything: it
// clears agent_proxy_listen and agent_proxy_vm_host, and only when the network exists and is host-only.
func (s *Setup) runNetwork(ctx context.Context, j *job, autoAddress bool) outcome {
	log := j.log(s)
	name := s.network()
	st := s.probeNetwork(ctx)
	if autoAddress {
		if st.State != Done && st.State != NeedsUser {
			if st.State == Todo {
				return outcome{Failed, msgNetworkApproval}
			}
			return outcome{st.State, st.Reason}
		}
		if err := s.opt.Update(func(c *Settings) { c.ProxyListen, c.ProxyVMHost = "", "" }); err != nil {
			return outcome{Failed, "Couldn't save the setting: " + safeText(err.Error())}
		}
		log("The proxy now uses the network's address.")
		st = s.probeNetwork(ctx)
		return outcome{st.State, st.Reason}
	}
	if st.State != Todo {
		return outcome{st.State, st.Reason} // done, or a network that isn't host-only: nothing is changed
	}
	log("Creating the host-only network " + name + "...")
	if out, err := s.opt.Cmd.Run(ctx, []string{"container", "network", "create", "--internal", name}); err != nil {
		if ctx.Err() != nil {
			return outcome{Todo, "Cancelled."}
		}
		return outcome{Failed, "Creating the network failed: " + safeText(string(out))}
	}
	st = s.probeNetwork(ctx)
	if st.State == Todo {
		return outcome{Failed, "The network was created but can't be inspected."}
	}
	return outcome{st.State, st.Reason}
}

func (s *Setup) runImage(ctx context.Context, j *job) outcome {
	log := j.log(s)
	if st := s.probeImage(ctx); st.State == Done {
		return outcome{Done, st.Reason}
	}
	vm := s.appleVM()
	argv, err := vm.BuildCommand()
	if err != nil {
		return outcome{Failed, safeText(err.Error())}
	}
	argv[0] = "container"
	log("Building " + vm.ImageTag() + " (this takes a few minutes)...")
	if err := s.opt.Cmd.Stream(ctx, argv, log); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return outcome{Todo, "Cancelled."}
		}
		s.mu.Lock()
		tail := j.prog.tail(3)
		s.mu.Unlock()
		return outcome{Failed, fmt.Sprintf("The build failed. %s", safeText(tail))}
	}
	if st := s.probeImage(ctx); st.State != Done {
		return outcome{Failed, "The build finished, but the image isn't there."}
	}
	return outcome{Done, "The runner image is built."}
}
