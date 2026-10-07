package mcpproxy

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestSessionRestoreAfterRestart(t *testing.T) {
	var out bytes.Buffer
	s := &session{out: bufio.NewWriter(&out), pending: map[string]bool{}}

	s.fromAgent([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	s.fromAgent([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if !s.fromDaemon([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)) {
		t.Fatal("first initialize reply hidden")
	}
	s.fromAgent([]byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{}}`))

	// The daemon restarts: the in-flight call gets an error reply, the handshake is replayed, and the
	// replayed handshake's reply is hidden from the agent.
	s.failPending()
	if !strings.Contains(out.String(), `"id":7`) || !strings.Contains(out.String(), "Retry it") {
		t.Fatalf("in-flight request not answered: %s", out.String())
	}
	var conn bytes.Buffer
	if err := s.replay(&conn); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conn.String(), `"method":"initialize"`) || !strings.Contains(conn.String(), "notifications/initialized") {
		t.Fatalf("handshake not replayed: %s", conn.String())
	}
	if s.fromDaemon([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)) {
		t.Fatal("replayed initialize reply shown to the agent")
	}
	if !s.fromDaemon([]byte(`{"jsonrpc":"2.0","id":8,"result":{}}`)) {
		t.Fatal("normal reply hidden")
	}
}
