package agent

import (
	"bufio"
	"encoding/base64"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
	"strings"
	"testing"
	"time"
)

func basic(tok string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("rubi:"+tok))
}

func connectVia(t *testing.T, m *netpipe.Mux, svc byte, request string) string {
	t.Helper()
	s, err := m.Open(svc)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := s.Write([]byte(request)); err != nil {
		return "write: " + err.Error()
	}
	line, err := bufio.NewReader(s).ReadString('\n')
	if err != nil {
		return "read: " + err.Error()
	}
	return strings.TrimSpace(line)
}
