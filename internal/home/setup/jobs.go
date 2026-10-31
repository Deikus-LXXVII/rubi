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

// runNetwork does nothing: VMs have no network, so there is none to create (the step is "done: not needed"). The
// "auto-address" option of older plugins is accepted and has no effect.
func (s *Setup) runNetwork(ctx context.Context, j *job) outcome {
	j.log(s)(networkNotNeeded)
	st := s.probeNetwork(ctx)
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
