//go:build linux

package guest

import (
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

// On Linux the leader's exit ends what it started: the group is killed while the leader is still unreaped.
func TestLeaderExitKillsItsGroupOnLinux(t *testing.T) {
	r := groupRig(t)
	r.srv.AwaitExit = awaitExit
	p, err := r.c.Exec(ctx5(t), sh("sleep 30 & echo $!"), nil)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(p)
	if code, err := p.Wait(); code != 0 || err != nil {
		t.Fatalf("%d %v", code, err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(pid) {
		t.Fatalf("the background process %d outlived its leader", pid)
	}
}
