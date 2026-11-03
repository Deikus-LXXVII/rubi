package t3vm

import _ "embed"

// HookScript is the PreToolUse hook that runs in the VM (mounted at HookVMPath).
//
//go:embed rubi-hook.mjs
var HookScript []byte
