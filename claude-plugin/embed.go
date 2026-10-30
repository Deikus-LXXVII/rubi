// Package claudeplugin embeds the Claude Code marketplace "rubi" (one plugin, rubi-sisters) that
// `rubi-home install-hook --with-claude-plugin` installs under /Library/Application Support/Rubi Home.
package claudeplugin

import "embed"

// Files is the marketplace directory: .claude-plugin/marketplace.json and rubi-sisters/ (its plugin.json,
// .mcp.json and skill).
//
//go:embed all:.claude-plugin all:rubi-sisters
var Files embed.FS
