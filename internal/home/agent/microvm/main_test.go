//go:build unix

package microvm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const confinedHelper = "RUBI_TEST_CONFINED"

// The test binary doubles as the programs the launcher starts, told apart by their command lines: a fake virtiofsd
// (--socket-path ...), a fake QEMU (-name ...), the confining helper (-fake-confine, the real ConfineMain) and an
// unconfined one for systems without Landlock (-fake-unconfined).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--socket-path":
			fakeVirtiofsd(os.Args[1:])
		case "-name":
			fakeQEMU(os.Args[1:])
		case "-fake-confine":
			err := ConfineMain(os.Args[2:], os.Getenv(ConfineEnv))
			fmt.Fprintln(os.Stderr, "confine:", err)
			os.Exit(2)
		case "-fake-unconfined":
			err := syscall.Exec(os.Args[3], os.Args[3:], []string{})
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	if v := os.Getenv(confinedHelper); v != "" {
		var job struct {
			Rules []FSRule
			Argv  []string
		}
		if err := json.Unmarshal([]byte(v), &job); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		err := ExecConfined(job.Rules, job.Argv, []string{"PATH=/usr/bin:/bin"})
		fmt.Fprintln(os.Stderr, "confined:", err)
		if errors.Is(err, ErrNoLandlock) {
			os.Exit(3)
		}
		os.Exit(2)
	}
	os.Exit(m.Run())
}

func argAfter(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

// fakeVirtiofsd serves its socket until its QEMU goes. A file ".fake" in the shared folder changes what it does: "fail"
// (a flag it doesn't take), "die-later", "escape" (it tries to write beside its folder and says so if it can).
func fakeVirtiofsd(args []string) {
	sock, dir := argAfter(args, "--socket-path"), argAfter(args, "--shared-dir")
	mode, _ := os.ReadFile(filepath.Join(dir, ".fake"))
	_ = os.WriteFile(filepath.Join(dir, ".fakepid"), []byte(fmt.Sprint(os.Getpid())), 0o600) // a read-only share refuses it
	switch strings.TrimSpace(string(mode)) {
	case "fail":
		fmt.Fprintln(os.Stderr, "error: unexpected argument '--bad-flag'")
		os.Exit(2)
	case "escape":
		if os.WriteFile(filepath.Join(filepath.Dir(dir), "escaped"), nil, 0o600) == nil {
			fmt.Fprintln(os.Stderr, "ESCAPED")
			os.Exit(1)
		}
	case "die-later":
		go func() { time.Sleep(300 * time.Millisecond); fmt.Fprintln(os.Stderr, "died"); os.Exit(1) }()
	case "chatty":
		fmt.Fprintln(os.Stderr, "WARN virtiofsd: something about the shared folder")
	}
	var l net.Listener
	if strings.TrimSpace(string(mode)) == "slow-listen" {
		l = bindThenListen(sock, 300*time.Millisecond) // as some virtiofsd versions: the file well before the listen
	} else {
		var err error
		if l, err = net.Listen("unix", sock); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	c, err := l.Accept()
	if err != nil {
		os.Exit(1)
	}
	_, _ = io.Copy(io.Discard, c) // QEMU gone: virtiofsd ends
	os.Exit(0)
}

// fakeQEMU writes its pid file, serves the console and control sockets, connects to every file server and says
// "booted" on the console. A file "fake-qemu.mode" in the VM's folder says how it ends: "exit0" (as the guest's restart
// does), "fail" (before any socket), "ignore-term" (only SIGKILL ends it), anything else: on SIGTERM.
func fakeQEMU(args []string) {
	var console, control, dir string
	var fs []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-chardev" {
			continue
		}
		var id, path string
		for _, kv := range strings.Split(args[i+1], ",") {
			if v, ok := strings.CutPrefix(kv, "id="); ok {
				id = v
			}
			if v, ok := strings.CutPrefix(kv, "path="); ok {
				path = v
			}
		}
		switch {
		case id == "console":
			console, dir = path, filepath.Dir(path)
		case id == "control":
			control = path
		default:
			fs = append(fs, path)
		}
	}
	mode, _ := os.ReadFile(filepath.Join(dir, "fake-qemu.mode"))
	m := strings.TrimSpace(string(mode))
	if m == "fail" {
		fmt.Fprintln(os.Stderr, "qemu-system-x86_64: -machine microvm: unsupported")
		os.Exit(1)
	}
	if m == "ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	}
	_ = os.WriteFile(argAfter(args, "-pidfile"), []byte(fmt.Sprintln(os.Getpid())), 0o600)
	var conns []net.Conn
	for _, p := range fs {
		c, err := net.Dial("unix", p)
		if err != nil {
			fmt.Fprintln(os.Stderr, "vhost-user:", err)
			os.Exit(1)
		}
		conns = append(conns, c)
	}
	if m == "die-after-bind" {
		_ = bindThenListen(console, 0)
		fmt.Fprintln(os.Stderr, "qemu-system-x86_64: cannot create memory backend")
		os.Exit(1)
	}
	cl, err := net.Listen("unix", control)
	if err != nil {
		os.Exit(1)
	}
	defer cl.Close()
	var ln net.Listener
	if m == "slow-listen" {
		ln = bindThenListen(console, 300*time.Millisecond) // as QEMU does, with a gap between the two
	} else if ln, err = net.Listen("unix", console); err != nil {
		os.Exit(1)
	}
	c, err := ln.Accept()
	if err != nil {
		os.Exit(1)
	}
	fmt.Fprintln(c, "booted")
	if m == "exit0-fs-first" { // as QEMU on its way out: the file servers' connections close first
		for _, fc := range conns {
			fc.Close()
		}
		time.Sleep(300 * time.Millisecond)
		os.Exit(0)
	}
	if m == "exit0" {
		time.Sleep(100 * time.Millisecond)
		os.Exit(0)
	}
	for {
		time.Sleep(time.Hour)
	}
}

// bindThenListen makes the socket file at once and listens only after gap.
func bindThenListen(path string, gap time.Duration) net.Listener {
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		err = syscall.Bind(fd, &syscall.SockaddrUnix{Name: path})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	time.Sleep(gap)
	if err := syscall.Listen(fd, 1); err != nil {
		os.Exit(1)
	}
	ln, err := net.FileListener(os.NewFile(uintptr(fd), path))
	if err != nil {
		os.Exit(1)
	}
	return ln
}
