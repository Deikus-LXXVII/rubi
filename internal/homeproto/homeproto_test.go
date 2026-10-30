package homeproto

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/e2e"
)

// An operation that never returns (a read stuck on a hanging disk) still gets an answer by the deadline:
// an error the caller can show, not silence until its own timeout.
func TestHandleRPCAnswersStuckOperationByDeadline(t *testing.T) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	stuck := make(chan struct{})
	defer close(stuck)
	srv := &Server{Key: key, Handle: func(context.Context, Envelope) (any, error) {
		<-stuck // ignores ctx, like a blocked open(2)
		return "late", nil
	}}
	env := Envelope{Op: "claude-sessions.list", TS: time.Now().Unix(), RID: RandomToken()}
	plain, _ := json.Marshal(env)
	req, respKey, err := e2e.Client{InstanceKey: key.PublicKey()}.Seal(Path, plain)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	status, resp := srv.HandleRPC(ctx, body)
	if took := time.Since(start); status != http.StatusOK || took > 2*time.Second {
		t.Fatalf("status %d after %s", status, took)
	}
	var sealed e2e.Response
	if err := json.Unmarshal(resp, &sealed); err != nil {
		t.Fatal(err)
	}
	opened, err := e2e.OpenResponse(respKey, env.RID, sealed)
	if err != nil {
		t.Fatal(err)
	}
	var r reply
	if err := json.Unmarshal(opened, &r); err != nil || r.OK || !strings.Contains(r.Error, "in time") {
		t.Fatalf("reply %s (%v)", opened, err)
	}
}
