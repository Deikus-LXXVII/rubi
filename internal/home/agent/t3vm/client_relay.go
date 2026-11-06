package t3vm

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"

	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
)

// relayProcess runs argv (argv[0] is the name the command is called by; path is the program) with the given input and output
// and returns its exit status. This is what the shim does where a process cannot be replaced by another (Windows): it stays
// alive, holding the connection to Home open for as long as the command runs, and passes the command's input, output and exit
// status through. It writes nothing of its own to the streams.
//
// The command runs in a job tied to the shim (fsx.RunTree): if the shim is killed, or ends, the command is killed with it, so
// no command outlives the connection that was the reason it may run. Ctrl-C is caught by the shim while the command runs: the
// command gets it (it shares the console), and the shim stays, holding the connection, until the command has ended.
func relayProcess(path string, argv, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(argv) < 1 {
		return 1, errors.New("no command")
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	intr := make(chan os.Signal, 1)
	signal.Notify(intr, os.Interrupt)
	defer signal.Stop(intr)
	if err := fsx.RunTree(cmd); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}
