package agent

import "log"

// EnforceLibraryRevocations rebuilds every role without the library versions that were withdrawn (the signed list
// Rubi's core pushed to Rubi Home; see home.setLibraryRevoked) and returns how many skills it took out. A VM start
// does the same for its own role (pinRole). It is logged and recorded in agent-roles/revocations.jsonl.
func (m *Manager) EnforceLibraryRevocations() int {
	if m.roles == nil {
		return 0
	}
	n := 0
	for _, row := range m.roles.List("") {
		left, err := m.roles.EnforceRevocations(row.ID)
		if err != nil {
			log.Printf("library revocations: role %s: %v", row.ID, err)
		}
		n += len(left)
	}
	return n
}
