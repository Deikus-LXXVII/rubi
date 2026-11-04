package home

import (
	"context"
	"runtime"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
)

// What this computer can do (docs/plans/home-platforms.md, stage P7). Some features exist on some systems only: Shortcuts
// need macOS, the Claude desktop app is for macOS and Windows, agents need a virtual machine backend. Home works out
// which are available, tells Rubi in `hello` (`can` lists only those, `features` gives each with its reason), and answers
// every operation of a feature that is not available with one stable error, not_available_here, with a plain sentence
// for the user. Nothing of an unavailable feature starts, fails halfway or shows a program's own error message.

// Feature is one thing Home may offer.
type Feature struct {
	Available bool `json:"available"`
	// Reason is why it is not available, as the inside of the sentence "That isn't available on this computer (...)." Empty when
	// it is available.
	Reason string `json:"reason,omitempty"`
}

// The features.
const (
	FeatureShortcuts   = "shortcuts"    // the Shortcuts app and the shortcuts command
	FeatureAppleHome   = "apple_home"   // Apple Home, which reaches Rubi through Shortcuts
	FeatureClaudeStart = "claude-start" // starting a Claude Code session through the Claude desktop app's link
	FeatureAgent       = "agent"        // agent sessions in a virtual machine (claude-agent, agent-setup)
	FeatureT3          = "t3"           // T3 Agents, which also run in a virtual machine (t3-agent, t3-setup)
)

// featureOrder is the order features are listed in.
var featureOrder = [...]string{FeatureShortcuts, FeatureAppleHome, FeatureClaudeStart, FeatureAgent, FeatureT3}

// FeatureLabel is the name of a feature in a sentence for the user.
func FeatureLabel(name string) string {
	switch name {
	case FeatureShortcuts:
		return "Shortcuts"
	case FeatureAppleHome:
		return "Apple Home"
	case FeatureClaudeStart:
		return "Starting Claude Code from the Claude app"
	case FeatureAgent:
		return "Agents"
	case FeatureT3:
		return "T3 Agents"
	}
	return name
}

// opFeatures is the feature each family of operations belongs to (the op's name up to the first dot). An operation of a
// family that is not here is not tied to a feature: Hue, the Claude Code hook, sessions and the folder browser work
// everywhere.
var opFeatures = map[string]string{
	"shortcuts":    FeatureShortcuts,
	"claude-start": FeatureClaudeStart,
	"claude-agent": FeatureAgent,
	"agent-setup":  FeatureAgent,
	"t3-agent":     FeatureT3,
	"t3-setup":     FeatureT3,
}

// canNames is what `hello` lists under "can": the capabilities Rubi may ask for, each with the feature it needs ("" for none).
var canNames = []struct{ name, feature string }{
	{name: "hue"},
	{name: "shortcuts", feature: FeatureShortcuts},
	{name: "claude"},
	{name: "claude-sessions"},
	{name: "claude-start", feature: FeatureClaudeStart},
	{name: "claude-agent", feature: FeatureAgent},
	{name: "agent-setup", feature: FeatureAgent},
	{name: "t3-agent", feature: FeatureT3},
	{name: "t3-setup", feature: FeatureT3},
}

// opFeature is the feature an operation needs ("" when none).
func opFeature(op string) string {
	if i := strings.IndexByte(op, '.'); i > 0 {
		return opFeatures[op[:i]]
	}
	return ""
}

// FeatureEnv is what the features are worked out from; tests give their own.
type FeatureEnv struct {
	GOOS    string
	Desktop Desktop // for the check that a program is registered for the claude:// link
	// Backend is the VM backend of this computer (agent.VMBackend); agents and T3 are available where it is. Nil is none.
	Backend agent.VMBackend
	// T3Platform is the platform key of T3's pins (t3Platform); T3 is available only where there is a build for it. "" means
	// do not check (a test of something else).
	T3Platform string
}

// systemFeatureEnv is the environment of the system Home runs on (a variable so that the tests of the operations, which run
// on every system, can run them as on a Mac, where everything is available).
var systemFeatureEnv = func() FeatureEnv {
	return FeatureEnv{GOOS: runtime.GOOS, Desktop: NewDesktop(DesktopEnv{}), Backend: agent.PlatformBackend(agent.BackendConfig{}),
		T3Platform: t3Platform()}
}

// ComputeFeatures says which features are available in env.
func ComputeFeatures(ctx context.Context, env FeatureEnv) map[string]Feature {
	mac := env.GOOS == "darwin"
	needMac := Feature{Reason: "it needs macOS"}
	out := map[string]Feature{}
	out[FeatureShortcuts], out[FeatureAppleHome] = needMac, needMac
	if mac {
		out[FeatureShortcuts], out[FeatureAppleHome] = Feature{Available: true}, Feature{Available: true}
	}
	switch {
	case mac:
		out[FeatureClaudeStart] = Feature{Available: true}
	case env.Desktop.Scheme == nil || env.Desktop.Opener == nil:
		out[FeatureClaudeStart] = Feature{Reason: "this computer has no desktop to open a link on"}
	case !env.Desktop.Scheme.Registered(ctx, "claude"):
		out[FeatureClaudeStart] = Feature{Reason: "the Claude desktop app isn't installed here"}
	default:
		out[FeatureClaudeStart] = Feature{Available: true}
	}
	// agents and T3 sessions run in VMs: they are available where the computer has a VM backend (agent.VMBackend), and the
	// backend says why not
	agents := Feature{Reason: "agents don't run on " + systemName(env.GOOS) + " yet"}
	if env.Backend != nil {
		if av := env.Backend.Available(ctx); av.OK {
			agents = Feature{Available: true}
		} else if av.Reason != "" {
			agents = Feature{Reason: av.Reason}
		}
	}
	out[FeatureAgent], out[FeatureT3] = agents, agents
	// T3 also needs a T3 build for this platform (upstream publishes none for Intel Macs)
	if agents.Available && env.T3Platform != "" {
		if pin, err := loadT3Pin(); err == nil {
			if why := t3Unavailable(pin, env.T3Platform); why != "" {
				out[FeatureT3] = Feature{Reason: why}
			}
		}
	}
	return out
}

func systemName(goos string) string {
	switch goos {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	}
	return "this system"
}

// featureTTL is how long a computed answer is kept. Only the check for the Claude desktop app can change while Home runs
// (the user installs the app); on a Mac nothing is checked at all.
var featureTTL = time.Minute

// Features is what this computer can do now.
func (h *Helper) Features() map[string]Feature {
	h.featMu.Lock()
	defer h.featMu.Unlock()
	if h.feat == nil || time.Since(h.featAt) >= featureTTL { // >=: a coarse clock (Windows) can say no time passed
		env := systemFeatureEnv
		if h.featureEnv != nil {
			env = h.featureEnv
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h.feat, h.featAt = ComputeFeatures(ctx, env()), time.Now()
	}
	out := make(map[string]Feature, len(h.feat))
	for k, v := range h.feat {
		out[k] = v
	}
	return out
}

// can is the list `hello` gives under "can": the capabilities whose feature is available.
func (h *Helper) can() []string {
	f := h.Features()
	var out []string
	for _, c := range canNames {
		if c.feature == "" || f[c.feature].Available {
			out = append(out, c.name)
		}
	}
	return out
}

// NotAvailableMessage is the sentence for the user.
func NotAvailableMessage(f Feature) string {
	if f.Reason == "" {
		return "That isn't available on this computer."
	}
	return "That isn't available on this computer (" + f.Reason + ")."
}

// notAvailable is the answer to an operation of a feature this computer does not have.
func notAvailable(f Feature) error {
	return &homeproto.CodedError{Code: homeproto.CodeNotAvailableHere, Message: NotAvailableMessage(f)}
}

// gate is nil when the operation may run, else the not_available_here error.
func (h *Helper) gate(op string) error {
	name := opFeature(op)
	if name == "" {
		return nil
	}
	if f := h.Features()[name]; !f.Available {
		return notAvailable(f)
	}
	return nil
}
