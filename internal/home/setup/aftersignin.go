package setup

import "log"

const (
	// SigningIn is the login row while the helper finishes after the code was accepted.
	SigningIn = "Signing in..."
	// SelfTestRunning is the self-test row while that step is in progress.
	SelfTestRunning = "Running the self-test."

	// Titles the setup status uses. The sandbox and the real Home both use these, so a
	// renamed row fails the test that compares them.
	TitleLogin    = "Claude sign-in"
	TitleT3       = "T3 installed, pinned and running"
	TitlePair     = "T3 paired with Rubi Home"
	TitleSelfTest = "Self-test"
)

// SignInPhases is what a client sees, in order, after the code is accepted: the sign-in is still
// working, then the self-test that sign-in completion started, then both are done.
func SignInPhases() []string {
	return []string{
		"login:working:" + SigningIn,
		"selftest:working:" + SelfTestRunning,
		"ready",
	}
}

// AfterSignIn starts the self-test because a sign-in completed. The real Home and the sandbox both
// call it, and neither starts the self-test from the code handler itself.
func AfterSignIn(start func() error) {
	if start == nil {
		return
	}
	if err := start(); err != nil {
		log.Printf("self-test after sign-in: %v", err)
	}
}
