.PHONY: sandbox-up sandbox-down sandbox-reset sandbox-check betatest

sandbox-up:
	scripts/sandbox/up.sh

sandbox-down:
	scripts/sandbox/down.sh

sandbox-reset:
	scripts/sandbox/reset.sh

sandbox-check:
	scripts/sandbox/check.sh

betatest:
	scripts/sandbox/betatest.sh
