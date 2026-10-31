package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Deikus-LXXVII/rubi/internal/library"
)

// Tools of the shared skill library (docs/design/agent-roles.md section 8). Core knows the calling Bot itself:
// a tool that writes needs agent and code (the agent_code of the Bot's latest Rubi webhook), and the curator
// tools also need the Bot to be the library's curator, which only the user sets.

type libraryAuth struct {
	Agent string `json:"agent,omitempty" jsonschema:"your Bot's name as connected to Rubi"`
	Code  string `json:"code,omitempty" jsonschema:"agent_code from your latest Rubi webhook (or from rubi_verify)"`
}

type libraryListIn struct {
	libraryAuth
	Query string `json:"query,omitempty" jsonschema:"text to find in the id, description or tags"`
	Tag   string `json:"tag,omitempty"`
}

type libraryGetIn struct {
	libraryAuth
	ID      string `json:"id"`
	Version int    `json:"version,omitempty" jsonschema:"default: the latest admitted version"`
}

type librarySubmitIn struct {
	libraryAuth
	ID          string   `json:"id,omitempty" jsonschema:"the skill's id (also its name:); default: the name in the content. An existing id proposes a new version"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
	Content     string   `json:"content" jsonschema:"the SKILL.md text, at most 32 KiB"`
}

type libraryAdmitIn struct {
	libraryAuth
	ProposalID string   `json:"proposal_id" jsonschema:"id@version, as rubi_library_submit returned it"`
	Ack        []string `json:"ack,omitempty" jsonschema:"every rule id the scan flagged, to say you read each one"`
}

type libraryUpdateIn struct {
	librarySubmitIn
	Ack []string `json:"ack,omitempty" jsonschema:"every rule id the scan flagged"`
}

type libraryStateIn struct {
	libraryAuth
	ID      string `json:"id"`
	Version int    `json:"version,omitempty" jsonschema:"default: every version that applies"`
}

func (s *Server) registerLibraryTools() {
	const curator = " Curator only: the user names the curator Bot in Rubi's settings."
	addCore(s, &mcp.Tool{Name: "rubi_library_list",
		Description: "List the shared skill library's admitted skills (id, description, tags, versions). Optional query and tag. The curator, passing agent and code, also sees proposals."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in libraryListIn) (*mcp.CallToolResult, out, error) {
			if locked := s.lockedResponse(); locked != nil {
				return nil, locked, nil
			}
			res, err := s.core.LibraryList(in.Agent, in.Code, in.Query, in.Tag)
			return nil, res, err
		})
	addCore(s, &mcp.Tool{Name: "rubi_library_get",
		Description: "Read one admitted skill version with its content and scan result (default: the latest admitted)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in libraryGetIn) (*mcp.CallToolResult, out, error) {
			if locked := s.lockedResponse(); locked != nil {
				return nil, locked, nil
			}
			info, err := s.core.LibraryGet(in.Agent, in.Code, in.ID, in.Version)
			if err != nil {
				return nil, nil, err
			}
			return nil, out{"skill": info}, nil
		})
	addCore(s, &mcp.Tool{Name: "rubi_library_submit",
		Description: "Propose a skill (or a new version of an existing one) for the shared library. The automatic scan runs at once (a reject is final); the curator then reviews it. Needs agent and code. Returns proposal_id and the scan."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in librarySubmitIn) (*mcp.CallToolResult, out, error) {
			if locked := s.lockedResponse(); locked != nil {
				return nil, locked, nil
			}
			p, err := s.core.LibrarySubmit(in.Agent, in.Code, library.Submission{ID: in.ID, Description: in.Description,
				Tags: in.Tags, Content: in.Content})
			if err != nil {
				return nil, nil, err
			}
			return nil, out{"proposal_id": p.ProposalID, "state": p.State, "scan": p.Scan}, nil
		})
	addCore(s, &mcp.Tool{Name: "rubi_library_admit",
		Description: "Admit a proposal into the library: Rubi signs it. The scan runs again and must pass; acknowledge every flagged rule id in ack. You may admit your own proposal (it is marked self-admitted and logged)." + curator},
		func(ctx context.Context, _ *mcp.CallToolRequest, in libraryAdmitIn) (*mcp.CallToolResult, out, error) {
			if locked := s.lockedResponse(); locked != nil {
				return nil, locked, nil
			}
			res, err := s.core.LibraryAdmit(in.Agent, in.Code, in.ProposalID, in.Ack)
			if err != nil {
				return nil, nil, err
			}
			return nil, out{"admitted": res}, nil
		})
	addCore(s, &mcp.Tool{Name: "rubi_library_update",
		Description: "Publish a new version of a skill as the curator: submitted and admitted in one step (marked self-admitted). Same checks as rubi_library_admit." + curator},
		func(ctx context.Context, _ *mcp.CallToolRequest, in libraryUpdateIn) (*mcp.CallToolResult, out, error) {
			if locked := s.lockedResponse(); locked != nil {
				return nil, locked, nil
			}
			res, err := s.core.LibraryUpdate(in.Agent, in.Code, library.Submission{ID: in.ID, Description: in.Description,
				Tags: in.Tags, Content: in.Content}, in.Ack)
			if err != nil {
				return nil, nil, err
			}
			return nil, out{"admitted": res}, nil
		})
	for _, t := range []struct {
		name, desc string
		to         library.State
	}{
		{"rubi_library_deprecate", "Deprecate a skill version (or every admitted one): roles that use it keep it, nothing new can attach it.", library.Deprecated},
		{"rubi_library_withdraw", "Withdraw a skill version (or all): roles drop it at their next materialization. To undo, admit the version again with rubi_library_admit.", library.Withdrawn},
	} {
		to := t.to
		addCore(s, &mcp.Tool{Name: t.name, Description: t.desc + curator},
			func(ctx context.Context, _ *mcp.CallToolRequest, in libraryStateIn) (*mcp.CallToolResult, out, error) {
				if locked := s.lockedResponse(); locked != nil {
					return nil, locked, nil
				}
				changed, err := s.core.LibrarySetState(in.Agent, in.Code, in.ID, in.Version, to)
				if err != nil {
					return nil, nil, err
				}
				return nil, out{"id": in.ID, "state": to, "versions": changed}, nil
			})
	}
}
