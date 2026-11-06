// Package t3 embeds the pin of the T3 Code release Rubi is built against and the contract snapshot
// (the tool names and input schemas of the T3 MCP server that the bridge uses).
package t3

import "embed"

// Upstream is upstream.json: the pinned package, version, commit and integrity.
//
//go:embed upstream.json
var Upstream []byte

// Contract holds the snapshot files, e.g. contract/tools.json.
//
//go:embed contract/*.json
var Contract embed.FS
