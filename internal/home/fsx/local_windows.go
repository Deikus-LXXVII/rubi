package fsx

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/Deikus-LXXVII/rubi/internal/home/hostpaths"
	"golang.org/x/sys/windows"
)

// sioAFUnixGetPeerPID is SIO_AF_UNIX_GETPEERPID, the ioctl that names the process on the other end of an AF_UNIX socket
// (_WSAIOR(IOC_VENDOR, 256)). A Windows without it answers an error, and the peer then counts as unknown, which is a refusal.
const sioAFUnixGetPeerPID = 0x58000100

// maxSocketPath is the room in sockaddr_un for a path (108 with the terminating zero).
const maxSocketPath = 107

// ListenLocal makes a local socket at path. Its folder is given an access list for the current user alone (and read back to
// be sure), a socket file left by an earlier run is removed, and every connection is checked as it is accepted: its process
// must be the same user's, in the same terminal session and logon (see local.go). The folder must be Home's own (see homeRoots).
func ListenLocal(path string) (net.Listener, error) {
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("the socket path is too long for a local socket (%d characters, at most %d)", len(path), maxSocketPath)
	}
	if err := privateFolder(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if !IsSocket(fi) {
			return nil, fmt.Errorf("%s exists and isn't a socket", path)
		}
		_ = os.Remove(path) // left over from an earlier run
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	return Checked(ln, peerCheck, log.Printf), nil
}

// DialLocalContext connects to a local socket and checks the server on the other end the same way.
func DialLocalContext(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	if err := peerCheck(c, time.Now()); err != nil {
		c.Close()
		return nil, fmt.Errorf("the server on %s is not this user's Rubi Home: %w", filepath.Base(path), err)
	}
	return c, nil
}

// IsSocket says whether a file is a local socket: Go reports a file with the AF_UNIX reparse tag as os.ModeSocket, and any other
// reparse point as os.ModeIrregular, which is not a socket and is refused.
func IsSocket(fi os.FileInfo) bool { return fi.Mode()&os.ModeSocket != 0 }

var (
	rootsMu    sync.Mutex
	extraRoots []string
)

// AllowSocketRoot lets ListenLocal make sockets in dir and below, besides Home's own folders. It is for tests, whose folders
// are temporary ones; Home itself never calls it.
func AllowSocketRoot(dir string) {
	rootsMu.Lock()
	defer rootsMu.Unlock()
	extraRoots = append(extraRoots, dir)
}

// homeRoots are the folders ListenLocal may restrict to the user: Home's data folder (the default, or the one RUBI_HOME_DIR
// names, which is what Home uses when it is set) and the helper folder. A socket anywhere else is refused, so that a path
// handed in by mistake can never get another folder's access list rewritten.
func homeRoots() []string {
	var roots []string
	if d := os.Getenv("RUBI_HOME_DIR"); d != "" {
		roots = append(roots, d)
	}
	if base, err := hostpaths.DataBase(); err == nil && base != "" {
		roots = append(roots, filepath.Join(base, "Rubi Home"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, hostpaths.HelperHome(home))
	}
	rootsMu.Lock()
	defer rootsMu.Unlock()
	return append(roots, extraRoots...)
}

// privateFolder makes dir and gives it an access list for the current user alone, then reads it back and checks it. dir must be
// inside one of Home's folders (homeRoots) and must not be a link: the folder is opened without following links, looked at on
// the open handle, and its access list is set through that handle, so a link put in its place can't redirect the change.
func privateFolder(dir string) error {
	if !InsideRoots(dir, homeRoots(), true) {
		return fmt.Errorf("%s is not inside Rubi Home's folders; a local socket is made only there", dir)
	}
	sid, _, err := currentUserSID()
	if err != nil {
		return err
	}
	sddl, err := UserOnlySDDL(sid)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return errors.New("the folder's access list can't be made")
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.READ_CONTROL|windows.WRITE_DAC, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return fmt.Errorf("can't open %s: %w", dir, err)
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || IsReparse(0, info.FileAttributes) {
		return fmt.Errorf("%s is a link or not a folder", dir)
	}
	if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("can't restrict %s to the current user: %w", dir, err)
	}
	_, null, aces, err := readACL(dir)
	if err != nil {
		return err
	}
	if ok, why := CheckUserOnlyACL(null, aces, sid); !ok {
		return fmt.Errorf("%s is not private to the current user after all: %s", dir, why)
	}
	return nil
}

var (
	selfOnce sync.Once
	selfWho  Who
	selfErr  error
)

// currentUserSID is this process's user (text form) and terminal session, read once; selfWho has the logon too.
func currentUserSID() (string, uint32, error) {
	w, err := currentWho()
	return w.SID, w.Session, err
}

func currentWho() (Who, error) {
	selfOnce.Do(func() {
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, windows.GetCurrentProcessId())
		if err != nil {
			selfErr = err
			return
		}
		defer windows.CloseHandle(h)
		selfWho, selfErr = whoOf(h)
	})
	return selfWho, selfErr
}

// tokenStatistics is TOKEN_STATISTICS; only the authentication id (the logon) is used.
type tokenStatistics struct {
	TokenID, AuthenticationID                                                                   windows.LUID
	ExpirationTime                                                                              int64
	TokenType, ImpersonationLevel, DynamicCharged, DynamicAvailable, GroupCount, PrivilegeCount uint32
	ModifiedID                                                                                  windows.LUID
}

// whoOf reads the user, terminal session and logon of an open process from its token; an error when any of them can't be read.
func whoOf(h windows.Handle) (Who, error) {
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		return Who{}, err
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return Who{}, err
	}
	var session, n uint32
	if err := windows.GetTokenInformation(tok, windows.TokenSessionId, (*byte)(unsafe.Pointer(&session)), 4, &n); err != nil {
		return Who{}, err
	}
	var st tokenStatistics
	if err := windows.GetTokenInformation(tok, windows.TokenStatistics, (*byte)(unsafe.Pointer(&st)), uint32(unsafe.Sizeof(st)), &n); err != nil {
		return Who{}, err
	}
	logon := uint64(uint32(st.AuthenticationID.HighPart))<<32 | uint64(st.AuthenticationID.LowPart)
	return Who{SID: u.User.Sid.String(), Session: session, Logon: logon}, nil
}

// PeerPID is the process id of the process on the other end of a local socket connection (SIO_AF_UNIX_GETPEERPID); an error
// where the system can't say.
func PeerPID(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, errors.New("not a local socket connection")
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid, n uint32
	var ierr error
	if err := rc.Control(func(fd uintptr) {
		ierr = windows.WSAIoctl(windows.Handle(fd), sioAFUnixGetPeerPID, nil, 0, (*byte)(unsafe.Pointer(&pid)), 4, &n, nil, 0)
	}); err != nil {
		return 0, err
	}
	if ierr != nil {
		return 0, fmt.Errorf("the system can't name the process on the other end: %w", ierr)
	}
	// pid starts at 0, so a non-zero value was written by the system; some Windows builds report 0 bytes for this ioctl
	if pid == 0 {
		return 0, fmt.Errorf("the system gave no process for the other end (%d bytes)", n)
	}
	return int(pid), nil
}

// PeerSelf is nil when the process on the other end of the connection is this user's, in this terminal session and logon.
func PeerSelf(c net.Conn) error { return peerCheck(c, time.Now()) }

// peerCheck decides about the process on the other end of c, connected at the time given. The peer's process is opened
// right after its id is read and everything is read from that handle (so the id can't change under the check), and it must
// have been created before the connection was made, which a process that took over the id of an exited peer was not.
func peerCheck(c net.Conn, accepted time.Time) error {
	me, merr := currentWho()
	if merr != nil {
		return errors.New("this process's own user can't be read")
	}
	pid, err := PeerPID(c)
	if err != nil {
		return PeerAllowed(false, Who{}, me)
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return PeerAllowed(false, Who{}, me)
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return PeerAllowed(false, Who{}, me)
	}
	if err := PeerBorn(time.Unix(0, created.Nanoseconds()), accepted); err != nil {
		return err
	}
	who, err := whoOf(h)
	if err != nil {
		return PeerAllowed(false, Who{}, me)
	}
	return PeerAllowed(true, who, me)
}

// ProcInfo is a process's parent, its creation time (an opaque value that changes when the pid is reused) and its program's
// name without the folder and the .exe, in lower case ("claude" for claude.exe).
func ProcInfo(pid int) (ppid int, start int64, name string, err error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, 0, "", err
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0, 0, "", err
	}
	img, err := imageOf(h)
	if err != nil {
		return 0, 0, "", err
	}
	parent, err := parentPID(uint32(pid))
	if err != nil {
		return 0, 0, "", err
	}
	return int(parent), int64(created.HighDateTime)<<32 | int64(created.LowDateTime), baseName(img), nil
}

// ProcessImage is the full path of a process's program.
func ProcessImage(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	return imageOf(h)
}

func imageOf(h windows.Handle) (string, error) {
	buf := make([]uint16, 32768)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}

func baseName(img string) string {
	if i := strings.LastIndexAny(img, `\/`); i >= 0 {
		img = img[i+1:]
	}
	return strings.TrimSuffix(strings.ToLower(img), ".exe")
}

// parentPID finds a process's parent in a snapshot of the processes.
func parentPID(pid uint32) (uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err := windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID == pid {
			return e.ParentProcessID, nil
		}
	}
	return 0, errors.New("the process is gone")
}
