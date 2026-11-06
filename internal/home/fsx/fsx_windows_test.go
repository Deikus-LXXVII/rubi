package fsx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// There is no owner on Windows: every caller must then treat the file as not an administrator's.
func TestOwnerOfSaysUnknown(t *testing.T) {
	fi, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := OwnerOf(fi); ok {
		t.Fatal("Windows reported an owner")
	}
}

// The system folder is owned by the system and Users can not write it; a folder of the test is the user's.
func TestTrustedByAdminOnWindows(t *testing.T) {
	sys := os.Getenv("SystemRoot")
	if sys == "" {
		t.Skip("no SystemRoot")
	}
	fi, err := os.Stat(sys + `\System32\cmd.exe`)
	if err != nil {
		t.Skip(err)
	}
	if ok, why := TrustedByAdmin(sys+`\System32\cmd.exe`, fi); !ok {
		t.Errorf("cmd.exe: %s", why)
	}
	dir := t.TempDir()
	fi, _ = os.Stat(dir)
	if ok, _ := TrustedByAdmin(dir, fi); ok {
		t.Error("a temporary folder of the user was trusted")
	}
	if _, why := TrustedByAdmin(dir+`\nowhere`, fi); why == "" {
		t.Error("a missing path was trusted")
	}
}

func systemRoot(t *testing.T) string {
	t.Helper()
	sys := os.Getenv("SystemRoot")
	if sys == "" {
		t.Skip("no SystemRoot")
	}
	return sys
}

// setDACL replaces the access list of path with a protected one: the given entries, nothing inherited.
func setDACL(t *testing.T, path string, grants map[string]windows.ACCESS_MASK) {
	t.Helper()
	var entries []windows.EXPLICIT_ACCESS
	for sidText, mask := range grants {
		sid, err := windows.StringToSid(sidText)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: mask,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(sid)},
		})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Skip("can't set the access list here:", err)
	}
}

// A folder with only Administrators and SYSTEM in its list is trusted (when the test runs as an administrator, so that
// the owner is Administrators); an explicit DELETE for Users makes it untrusted, as a folder above the hook.
func TestAncestorWithUsersDeleteIsUntrusted(t *testing.T) {
	dir := t.TempDir()
	setDACL(t, dir, map[string]windows.ACCESS_MASK{SIDAdministrators: 0x1f01ff, SIDSystem: 0x1f01ff})
	fi, _ := os.Stat(dir)
	if ok, why := TrustedAncestor(dir, fi); !ok {
		t.Skip("the folder is not an administrator's here, so the test can't start from a trusted one:", why)
	}
	setDACL(t, dir, map[string]windows.ACCESS_MASK{SIDAdministrators: 0x1f01ff, SIDSystem: 0x1f01ff, "S-1-5-32-545": 0x10000})
	ok, why := TrustedAncestor(dir, fi)
	if ok || !strings.Contains(why, "S-1-5-32-545") {
		t.Errorf("Users with DELETE: %v %q", ok, why)
	}
	if ok, _ := TrustedByAdmin(dir, fi); ok {
		t.Error("Users with DELETE: the folder itself was trusted")
	}
}

// The drive root lets Authenticated Users add folders; for a folder above the hook that is acceptable.
func TestDriveRootIsAcceptableAsAnAncestor(t *testing.T) {
	root := filepath.VolumeName(systemRoot(t)) + `\`
	fi, err := os.Stat(root)
	if err != nil {
		t.Skip(err)
	}
	if ok, why := TrustedAncestor(root, fi); !ok {
		t.Errorf("%s as an ancestor: %s", root, why)
	}
}

func TestJunctionIsRefused(t *testing.T) {
	sys := systemRoot(t)
	link := filepath.Join(t.TempDir(), "junction")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, sys).CombinedOutput(); err != nil {
		t.Skip("no junctions here:", err, string(out))
	}
	li, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if !Reparse(link, li) {
		t.Error("a junction was not taken for a link")
	}
	fi, _ := os.Stat(link)
	if ok, why := TrustedByAdmin(link, fi); ok || !strings.Contains(why, "junction") {
		t.Errorf("the junction to the Windows folder: %v %q", ok, why)
	}
	if ok, _ := TrustedAncestor(link, fi); ok {
		t.Error("a junction as an ancestor was accepted")
	}
	// the real folder is not a link, and the system drive keeps permissions
	li, _ = os.Lstat(sys)
	if Reparse(sys, li) {
		t.Error("the Windows folder was taken for a link")
	}
	if ok, why := volumeOK(sys); !ok {
		t.Errorf("the system drive: %s", why)
	}
}

// The final path of an open folder is the real one, in the ordinary form, even when it was opened through a junction.
func TestFinalPathFollowsAJunction(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "junction")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skip("no junctions here:", err, string(out))
	}
	f, err := os.Open(link)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := FinalPath(f)
	want, _ := filepath.EvalSymlinks(target)
	if err != nil || !strings.EqualFold(got, want) || strings.HasPrefix(got, `\\?\`) {
		t.Fatalf("got %q (%v), want %q", got, err, want)
	}
}

// ---- local sockets ----

func shortTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rl") // a socket path is limited to 107 characters
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	AllowSocketRoot(dir) // ListenLocal works only inside Home's folders; this is a test's
	return dir
}

func whoOfPID(pid uint32) (Who, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return Who{}, err
	}
	defer windows.CloseHandle(h)
	return whoOf(h)
}

// The folder of the socket is the user's alone (read back from Windows, not assumed), a connection from this very process is
// accepted, and the system names this process as its peer, with this user and this logon session.
func TestListenLocalIsUserOnlyAndAcceptsTheUser(t *testing.T) {
	dir := filepath.Join(shortTemp(t), "home")
	path := filepath.Join(dir, "s.sock")
	ln, err := ListenLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	sid, session, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	_, null, aces, err := readACL(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := CheckUserOnlyACL(null, aces, sid); !ok {
		t.Fatalf("the socket's folder: %s (%+v)", why, aces)
	}
	pids := make(chan int, 1)
	errs := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			errs <- err
			return
		}
		defer c.Close()
		pid, err := PeerPID(c)
		if err != nil {
			errs <- err
			return
		}
		pids <- pid
	}()
	c, err := DialLocalContext(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case pid := <-pids:
		if pid != os.Getpid() {
			t.Fatalf("peer pid %d, want %d", pid, os.Getpid())
		}
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("no peer")
	}
	if err := PeerSelf(c); err != nil {
		t.Fatalf("the server on the other end of our own socket: %v", err)
	}
	got, err := whoOfPID(uint32(os.Getpid()))
	mine, _ := currentWho()
	if err != nil || got != mine || got.SID != sid || got.Session != session || got.Logon == 0 {
		t.Fatalf("%+v %v (ours %+v)", got, err, mine)
	}
	// the file the system made is a socket (the AF_UNIX reparse tag), and nothing else is
	if fi, err := os.Lstat(path); err != nil || !IsSocket(fi) {
		t.Fatalf("the socket file is not a socket: %v %v", fi, err)
	}
	if fi, err := os.Lstat(dir); err != nil || IsSocket(fi) {
		t.Fatalf("a folder is a socket: %v %v", fi, err)
	}
	// a stale socket file is replaced, a plain file in its place is not
	ln.Close()
	if ln2, err := ListenLocal(path); err != nil {
		t.Fatalf("a stale socket: %v", err)
	} else {
		ln2.Close()
	}
	os.Remove(path)
	os.WriteFile(path, []byte("x"), 0o600)
	if ln3, err := ListenLocal(path); err == nil {
		ln3.Close()
		t.Fatal("replaced a plain file")
	}
}

// A process of another user or another logon session is not the peer: services.exe runs as SYSTEM in session 0. Either it
// cannot be opened (unknown, refused) or its user and session differ (refused).
func TestAnotherUsersProcessIsRefused(t *testing.T) {
	pid := findProcess(t, "services.exe")
	me, err := currentWho()
	if err != nil {
		t.Fatal(err)
	}
	who, err := whoOfPID(pid)
	if err == nil && who == me {
		t.Fatalf("services.exe looks like this user's process: %+v", who)
	}
	if err := PeerAllowed(err == nil, who, me); err == nil {
		t.Fatalf("services.exe (%+v, error %v) was allowed", who, err)
	}
	// a pid that is not a process is unknown
	if _, err := whoOfPID(0xFFFFFFF0); err == nil {
		t.Error("a process that does not exist has a user")
	}
	if _, _, _, err := ProcInfo(0x7FFFFFF0); err == nil {
		t.Error("a process that does not exist has an info")
	}
}

func findProcess(t *testing.T, name string) uint32 {
	t.Helper()
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Skip(err)
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err := windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if strings.EqualFold(windows.UTF16ToString(e.ExeFile[:]), name) {
			return e.ProcessID
		}
	}
	t.Skip(name + " is not running")
	return 0
}

func TestProcInfoNamesThisProcess(t *testing.T) {
	ppid, start, name, err := ProcInfo(os.Getpid())
	if err != nil || ppid != os.Getppid() || start == 0 || name == "" || strings.Contains(name, ".exe") {
		t.Fatalf("%d %d %q %v", ppid, start, name, err)
	}
	img, err := ProcessImage(os.Getpid())
	self, _ := os.Executable()
	if err != nil || !strings.EqualFold(img, self) {
		t.Fatalf("%q %v, want %q", img, err, self)
	}
}

// ---- links ----

func TestOpenNoFollowRefusesAJunctionAndASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	os.WriteFile(target, []byte("x"), 0o600)
	// the plain file opens, and a new one is made
	f, err := OpenNoFollow(target, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("a plain file: %v", err)
	}
	f.Close()
	nf, err := OpenNoFollow(filepath.Join(dir, "new.txt"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	nf.Close()
	if f, err := OpenNoFollow(filepath.Join(dir, "new.txt"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
		f.Close()
		t.Fatal("O_EXCL opened an existing file")
	}
	// a junction to a folder
	folder := filepath.Join(dir, "folder")
	os.Mkdir(folder, 0o755)
	junction := filepath.Join(dir, "junction")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, folder).CombinedOutput(); err != nil {
		t.Skip("no junctions here:", err, string(out))
	}
	if f, err := OpenNoFollow(junction, os.O_RDONLY, 0); err == nil {
		f.Close()
		t.Fatal("a junction was followed")
	}
	// a symbolic link to a file (needs a privilege or developer mode)
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Log("no symbolic links here:", err)
		return
	}
	if f, err := OpenNoFollow(link, os.O_RDONLY, 0); err == nil {
		f.Close()
		t.Fatal("a symbolic link was followed")
	}
}

// ---- process trees ----

// A command that starts a grandchild: cancelling kills both (the job object), and the grandchild is gone afterwards.
func TestKillTreeKillsGrandchildren(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	ps := os.Getenv("SystemRoot") + `\System32\WindowsPowerShell\v1.0\powershell.exe`
	script := `$p = Start-Process -PassThru -WindowStyle Hidden -FilePath ping.exe -ArgumentList '-n','120','127.0.0.1'; Set-Content -Path '` + pidFile + `' -Value $p.Id; Start-Sleep -Seconds 120`
	for _, run := range []string{"RunTree (job object)", "plain Start (descendants by parent links)"} {
		os.Remove(pidFile)
		ctx, cancel := context.WithCancel(context.Background())
		cmd := exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-Command", script)
		KillTree(cmd)
		done := make(chan error, 1)
		if strings.HasPrefix(run, "RunTree") {
			go func() { done <- RunTree(cmd) }()
		} else {
			go func() { done <- cmd.Run() }()
		}
		var gpid uint32
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if b, err := os.ReadFile(pidFile); err == nil {
				if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n > 0 {
					gpid = uint32(n)
					break
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		if gpid == 0 {
			cancel()
			t.Fatalf("%s: the grandchild did not start", run)
		}
		if !alive(gpid) {
			cancel()
			t.Fatalf("%s: the grandchild is not running before the cancel", run)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: the command did not end after the cancel", run)
		}
		for i := 0; i < 50 && alive(gpid); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		if alive(gpid) {
			t.Fatalf("%s: the grandchild %d outlived the cancel", run, gpid)
		}
	}
}

func alive(pid uint32) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == 259 // STILL_ACTIVE
}

// ---- the second round of checks (stage P6 review) ----

// A path given to ListenLocal outside Rubi Home's folders is refused, and its access list is left as it was.
func TestListenLocalRefusesAFolderOutsideHome(t *testing.T) {
	dir, err := os.MkdirTemp("", "rl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	_, nullBefore, before, err := readACL(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ln, err := ListenLocal(filepath.Join(dir, "s.sock")); err == nil {
		ln.Close()
		t.Fatal("a socket was made outside Home's folders")
	}
	_, nullAfter, after, err := readACL(dir)
	if err != nil || nullBefore != nullAfter || len(before) != len(after) {
		t.Fatalf("the folder's access list changed: %v %d -> %d", err, len(before), len(after))
	}
}

// A folder that is a junction is refused, and the access list of the folder it points to is not touched.
func TestListenLocalRefusesAJunctionFolder(t *testing.T) {
	root := shortTemp(t)
	target := filepath.Join(root, "target")
	os.Mkdir(target, 0o700)
	junction := filepath.Join(root, "j")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
		t.Skip("no junctions here:", err, string(out))
	}
	_, _, before, err := readACL(target)
	if err != nil {
		t.Fatal(err)
	}
	if ln, err := ListenLocal(filepath.Join(junction, "s.sock")); err == nil {
		ln.Close()
		t.Fatal("a socket was made in a junction")
	}
	_, _, after, err := readACL(target)
	if err != nil || len(before) != len(after) {
		t.Fatalf("the target's access list changed: %v %d -> %d", err, len(before), len(after))
	}
}

// A reparse point that is not a socket (a junction) is not taken for one.
func TestIsSocketIsNotEveryReparsePoint(t *testing.T) {
	dir := t.TempDir()
	junction := filepath.Join(dir, "j")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, dir).CombinedOutput(); err != nil {
		t.Skip("no junctions here:", err, string(out))
	}
	fi, err := os.Lstat(junction)
	if err != nil || IsSocket(fi) {
		t.Fatalf("a junction counts as a socket: %v %v", fi.Mode(), err)
	}
}

// A link at the path is refused before anything is truncated: the target keeps its content. A new file is truncated through
// the handle, sharing does not allow a delete while it is open, and a folder is not opened.
func TestOpenNoFollowDoesNotTruncateThroughALink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	os.WriteFile(target, []byte("precious"), 0o600)
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Log("no symbolic links here:", err)
	} else {
		for _, flag := range []int{os.O_WRONLY | os.O_TRUNC, os.O_RDWR | os.O_CREATE | os.O_TRUNC, os.O_WRONLY | os.O_CREATE} {
			if f, err := OpenNoFollow(link, flag, 0o600); err == nil {
				f.Close()
				t.Fatalf("flag %#x followed a link", flag)
			}
		}
		if b, _ := os.ReadFile(target); string(b) != "precious" {
			t.Fatalf("the target was changed through the link: %q", b)
		}
	}
	// the plain file is truncated by O_TRUNC
	f, err := OpenNoFollow(target, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err == nil {
		t.Error("the file could be deleted while open: delete sharing is on")
	}
	f.Close()
	if b, _ := os.ReadFile(target); len(b) != 0 {
		t.Fatalf("O_TRUNC left %q", b)
	}
	if f, err := OpenNoFollow(dir, os.O_RDONLY, 0); err == nil {
		f.Close()
		t.Error("a folder was opened")
	}
}

// ---- job objects ----

// The job StartTree made is let go of when the command is waited for, by WaitTree and by RunTree, and by ReleaseTree; letting
// go terminates what is left in it.
func TestStartTreeJobIsReleased(t *testing.T) {
	cmdExe := os.Getenv("SystemRoot") + `\System32\cmd.exe`
	quick := exec.Command(cmdExe, "/c", "exit 0")
	if err := StartTree(quick); err != nil {
		t.Fatal(err)
	}
	if _, ok := jobs.Load(quick); !ok {
		t.Log("no job (the system refused one); nothing to release")
	}
	if err := WaitTree(quick); err != nil {
		t.Fatal(err)
	}
	if _, ok := jobs.Load(quick); ok {
		t.Error("WaitTree kept the job")
	}
	run := exec.Command(cmdExe, "/c", "exit 0")
	if err := RunTree(run); err != nil {
		t.Fatal(err)
	}
	if _, ok := jobs.Load(run); ok {
		t.Error("RunTree kept the job")
	}
	// ReleaseTree ends a command that is still running
	long := exec.Command(os.Getenv("SystemRoot")+`\System32\ping.exe`, "-n", "120", "127.0.0.1")
	if err := StartTree(long); err != nil {
		t.Fatal(err)
	}
	pid := uint32(long.Process.Pid)
	if _, ok := jobs.Load(long); !ok {
		t.Skip("no job object here")
	}
	ReleaseTree(long)
	ReleaseTree(long) // again: nothing
	for i := 0; i < 100 && alive(pid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if alive(pid) {
		t.Error("ReleaseTree left the command running")
	}
	long.Wait()
}

// The test binary is also the parent and the grandchild of the next test.
func TestTreeHelperParts(t *testing.T) {
	switch os.Getenv("TREE_PART") {
	case "parent": // starts a grandchild at once, then waits
		exe, _ := os.Executable()
		gc := exec.Command(exe, "-test.run=TestTreeHelperParts")
		gc.Env = append(os.Environ(), "TREE_PART=grandchild")
		_ = gc.Start()
		time.Sleep(time.Minute)
	case "grandchild":
		_ = os.WriteFile(os.Getenv("TREE_PIDFILE"), []byte(strconv.Itoa(os.Getpid())), 0o600)
		time.Sleep(time.Minute)
	}
}

// A command that starts a grandchild at its very first moment: the job holds it from the start (the process is created
// suspended, put in the job, then resumed) and the walk over the process table after the job is terminated finds any other. Over
// many tries, not one grandchild outlives the cancel.
func TestKillTreeCatchesAGrandchildStartedAtOnce(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for try := 0; try < 15; try++ {
		pidFile := filepath.Join(t.TempDir(), "gc.pid")
		ctx, cancel := context.WithCancel(context.Background())
		cmd := exec.CommandContext(ctx, exe, "-test.run=TestTreeHelperParts")
		cmd.Env = append(os.Environ(), "TREE_PART=parent", "TREE_PIDFILE="+pidFile)
		KillTree(cmd)
		done := make(chan error, 1)
		if err := StartTree(cmd); err != nil {
			cancel()
			t.Fatal(err)
		}
		go func() { done <- WaitTree(cmd) }()
		var gpid uint32
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if b, err := os.ReadFile(pidFile); err == nil {
				if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n > 0 {
					gpid = uint32(n)
					break
				}
			}
		}
		if gpid == 0 {
			cancel()
			t.Fatalf("try %d: the grandchild did not start", try)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("try %d: the command did not end after the cancel", try)
		}
		for i := 0; i < 100 && alive(gpid); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		if alive(gpid) {
			t.Fatalf("try %d: the grandchild %d outlived the cancel", try, gpid)
		}
	}
}
