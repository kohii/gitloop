package gitcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultRemoteTerminationGrace = 5 * time.Second
	remotePipeWait                = time.Second
)

// Result is the structured outcome of a single git invocation.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Error is returned when a git invocation exits non-zero or otherwise fails
// to run. It carries the structured Result so callers can inspect stderr
// without string-parsing an error message.
type Error struct {
	Args   []string
	Result Result
	Err    error
}

func (e *Error) Error() string {
	return fmt.Sprintf("git %s: %v (stderr: %s)", strings.Join(e.Args, " "), e.Err, strings.TrimSpace(e.Result.Stderr))
}

func (e *Error) Unwrap() error { return e.Err }

// Runner executes git subcommands against a single repository's working
// directory.
type Runner struct {
	// Dir is the repository's working directory.
	Dir string
	// executable is overridden by process-lifecycle tests. Production always
	// uses the git found on PATH.
	executable string
	// terminationGrace is shortened by process-lifecycle tests. Zero selects
	// the production default.
	terminationGrace time.Duration
}

// New returns a Runner for the repository checked out at dir.
func New(dir string) *Runner {
	return &Runner{Dir: dir}
}

// run executes `git <args...>` in r.Dir and returns its structured result.
//
// GIT_TERMINAL_PROMPT=0 is always set: without it, a git call that needs
// credentials it doesn't have would block on an interactive prompt with
// nothing attached to answer it, hanging the daemon indefinitely.
func (r *Runner) run(args ...string) (Result, error) {
	cmd := exec.Command(r.executablePath(), args...)
	return r.runCommand(cmd, args)
}

// runRemote executes a network-facing git command and follows ctx for its
// entire lifetime. Git may delegate transport to children such as ssh, so a
// canceled command terminates the new process group rather than only the
// direct git process.
func (r *Runner) runRemote(ctx context.Context, args ...string) (Result, error) {
	grace := r.remoteTerminationGrace()
	cmd := exec.CommandContext(ctx, r.executablePath(), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = grace + remotePipeWait

	commandDone := make(chan struct{})
	var cancellationStarted atomic.Bool
	cmd.Cancel = func() error {
		cancellationStarted.Store(true)
		if cmd.Process == nil {
			return os.ErrProcessDone
		}

		pid := cmd.Process.Pid
		if err := signalProcessGroup(pid, syscall.SIGTERM); err != nil {
			return err
		}
		go killProcessGroupAfter(pid, commandDone, grace)
		return nil
	}

	res, runErr := r.runCommand(cmd, args)
	// The direct git process can exit on SIGTERM before a transport child that
	// ignored the same signal. Once git itself is gone there is no useful
	// graceful work left for those descendants to perform, so reap any
	// surviving members before disarming the delayed group kill.
	if cancellationStarted.Load() && cmd.Process != nil {
		_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
	}
	close(commandDone)
	if ctxErr := ctx.Err(); cancellationStarted.Load() && ctxErr != nil && runErr != nil {
		var gitErr *Error
		if errors.As(runErr, &gitErr) {
			gitErr.Err = errors.Join(ctxErr, gitErr.Err)
		}
	}
	return res, runErr
}

func (r *Runner) remoteTerminationGrace() time.Duration {
	if r.terminationGrace > 0 {
		return r.terminationGrace
	}
	return defaultRemoteTerminationGrace
}

func (r *Runner) executablePath() string {
	if r.executable != "" {
		return r.executable
	}
	return "git"
}

func signalProcessGroup(pid int, signal syscall.Signal) error {
	err := syscall.Kill(-pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func killProcessGroupAfter(pid int, done <-chan struct{}, grace time.Duration) {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
		_ = signalProcessGroup(pid, syscall.SIGKILL)
	}
}

func (r *Runner) runCommand(cmd *exec.Cmd, args []string) (Result, error) {
	cmd.Dir = r.Dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
	}
	if runErr != nil {
		return res, &Error{Args: args, Result: res, Err: runErr}
	}
	return res, nil
}
