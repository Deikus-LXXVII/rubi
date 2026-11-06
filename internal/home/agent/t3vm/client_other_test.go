//go:build !unix

package t3vm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
)

// Where there is no exec, Run relays: it starts the command through Relay with the argv Home named, and the connection to Home
// stays open until the command ends and is closed after.
func TestRunRelaysWhereThereIsNoExec(t *testing.T) {
	dir, err := os.MkdirTemp("", "rl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s.sock")
	fsx.AllowSocketRoot(filepath.Dir(sock))
	ln, err := fsx.ListenLocal(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	closedAt := make(chan time.Time, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		br := bufio.NewReader(c)
		_, _ = br.ReadBytes('\n')
		b, _ := json.Marshal(StartReply{OK: true, ExecArgv: []string{`C:\fake\container`, "exec", "-i", "x", "claude"}})
		c.Write(append(b, '\n'))
		_, _ = io.Copy(io.Discard, c) // until the shim closes the connection
		closedAt <- time.Now()
	}()
	var gotPath string
	var gotArgv []string
	ran := make(chan time.Time, 1)
	o := ClientOptions{ExecTool: "container", Getwd: func() (string, error) { return `C:\work`, nil }, Environ: func() []string { return nil },
		Relay: func(path string, argv, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			gotPath, gotArgv = path, argv
			time.Sleep(200 * time.Millisecond) // the command runs; the connection must still be open
			ran <- time.Now()
			return 3, nil
		}}
	var out, errb bytes.Buffer
	code := Run(sock, "r1", []string{"--print"}, &out, &errb, o)
	if code != 3 || gotPath != `C:\fake\container` || len(gotArgv) != 5 || gotArgv[1] != "exec" {
		t.Fatalf("code %d path %q argv %q (stderr %q)", code, gotPath, gotArgv, errb.String())
	}
	select {
	case closed := <-closedAt:
		if ranAt := <-ran; closed.Before(ranAt) {
			t.Fatal("the connection to Home was closed before the command ended")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the connection to Home was never closed")
	}
}
