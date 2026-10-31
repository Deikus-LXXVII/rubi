package roles

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// Handle serves role.list, role.get, role.history, role.diff and role.put (op without the "claude-agent."
// prefix). Arguments are strict: an unknown key is an error.
func (s *Store) Handle(op string, args json.RawMessage) (any, error) {
	dec := func(v any) error {
		if len(args) == 0 || string(args) == "null" {
			args = json.RawMessage(`{}`)
		}
		d := json.NewDecoder(bytesReader(args))
		d.DisallowUnknownFields()
		if err := d.Decode(v); err != nil {
			return fmt.Errorf("bad arguments: %s", clipText(err.Error(), 120))
		}
		return nil
	}
	switch op {
	case "role.list":
		var in struct {
			Project string `json:"project"`
		}
		if err := dec(&in); err != nil {
			return nil, err
		}
		return map[string]any{"roles": s.List(in.Project)}, nil
	case "role.get":
		var in struct {
			ID      string `json:"id"`
			Version int    `json:"version"`
		}
		if err := dec(&in); err != nil {
			return nil, err
		}
		return s.Get(in.ID, in.Version)
	case "role.history":
		var in struct {
			ID    string `json:"id"`
			Limit int    `json:"limit"`
		}
		if err := dec(&in); err != nil {
			return nil, err
		}
		return s.History(in.ID, in.Limit)
	case "role.diff":
		var in struct {
			ID   string `json:"id"`
			From int    `json:"from"`
			To   int    `json:"to"`
		}
		if err := dec(&in); err != nil {
			return nil, err
		}
		return s.Diff(in.ID, in.From, in.To)
	case "role.put":
		var in PutArgs
		if err := dec(&in); err != nil {
			return nil, err
		}
		return s.Put(in)
	}
	return nil, fmt.Errorf("Rubi Home doesn't do %q", op)
}

// List returns every stored role (of one project when project is set) and each project's implicit legacy role
// "default" (id = the project id, not stored).
func (s *Store) List(project string) []Row {
	rows := []Row{}
	if s.opt.Projects != nil {
		for _, p := range s.opt.Projects() {
			if project == "" || project == p {
				rows = append(rows, Row{ID: p, Project: p, Name: "default", Status: "active", Legacy: true,
					Description: "The project's own .claude (shared with everyone working in the repo, not isolated)"})
			}
		}
	}
	for _, ix := range s.allIndexes() {
		if project == "" || ix.Project == project {
			rows = append(rows, Row{ID: ix.ID, Project: ix.Project, Name: ix.Name, Description: ix.Description,
				Status: ix.Status, Version: ix.Version, ChangedBy: ix.ChangedBy, ChangedAt: ix.ChangedAt})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		if a.Legacy != b.Legacy {
			return a.Legacy
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
	return rows
}

// current is the stored current version number.
func (s *Store) current(id string) (int, error) {
	if !IsID(id) {
		return 0, errors.New("bad role id")
	}
	ix, err := s.readIndex(id)
	if err != nil {
		if errors.Is(err, errNoRole) {
			return 0, fmt.Errorf("no role %q", id)
		}
		return 0, err
	}
	return ix.Version, nil
}

// Get returns one version of a role (version 0: the current one).
func (s *Store) Get(id string, version int) (*Role, error) {
	cur, err := s.current(id)
	if err != nil {
		return nil, err
	}
	if version == 0 {
		version = cur
	}
	return s.readVersion(id, version)
}

// HistoryEntry is one line of role.history.
type HistoryEntry struct {
	Version   int     `json:"version"`
	Parent    int     `json:"parent"`
	ChangedBy string  `json:"changed_by,omitempty"`
	ChangedAt string  `json:"changed_at"`
	Name      string  `json:"name"`
	Status    string  `json:"status"`
	Change    *Change `json:"change,omitempty"`
}

// History lists versions, newest first (limit 0: 20, at most MaxHistory).
func (s *Store) History(id string, limit int) (map[string]any, error) {
	cur, err := s.current(id)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, MaxHistory)
	out := []HistoryEntry{}
	for n := cur; n >= 1 && len(out) < limit; n-- {
		r, err := s.readVersion(id, n)
		if err != nil {
			return nil, err
		}
		out = append(out, HistoryEntry{Version: n, Parent: r.Parent, ChangedBy: r.ChangedBy, ChangedAt: r.ChangedAt,
			Name: r.Name, Status: r.Status, Change: r.Change})
	}
	return map[string]any{"id": id, "current": cur, "versions": out}, nil
}
