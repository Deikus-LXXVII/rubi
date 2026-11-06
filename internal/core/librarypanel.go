package core

import (
	"errors"

	"github.com/Deikus-LXXVII/rubi/internal/library"
)

// What the panel shows of the library (docs/design/agent-roles.md section 8). The owner sees every version in
// every state. There is no write path here: admitting, publishing, deprecating and withdrawing are the curator
// Bot's, and the only thing the user changes is who the curator is (RequestLibraryCurator).

// LibraryPanelList lists the skills (query and tag filter) with the current curator.
func (c *Core) LibraryPanelList(query, tag string) (map[string]any, error) {
	if err := c.libraryUnlocked(); err != nil {
		return nil, err
	}
	list, err := c.Library.List(query, tag, true)
	if err != nil {
		return nil, err
	}
	return map[string]any{"skills": list, "curator": c.LibraryCurator()}, nil
}

// LibraryPanelEntry returns every version of one skill, without content.
func (c *Core) LibraryPanelEntry(id string) (map[string]any, error) {
	if err := c.libraryUnlocked(); err != nil {
		return nil, err
	}
	v, err := c.Library.Entry(id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "versions": v, "curator": c.LibraryCurator()}, nil
}

// LibraryPanelVersion returns one version with its content (version 0: the latest admitted).
func (c *Core) LibraryPanelVersion(id string, version int) (library.Info, error) {
	if err := c.libraryUnlocked(); err != nil {
		return library.Info{}, err
	}
	if version < 0 {
		return library.Info{}, errors.New("bad version")
	}
	return c.Library.Get(id, version, true)
}

// LibraryPanelQueue returns the proposals waiting for the curator.
func (c *Core) LibraryPanelQueue() (map[string]any, error) {
	if err := c.libraryUnlocked(); err != nil {
		return nil, err
	}
	q, err := c.Library.Queue()
	if err != nil {
		return nil, err
	}
	return map[string]any{"proposals": q, "curator": c.LibraryCurator()}, nil
}
