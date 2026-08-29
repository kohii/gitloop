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

// runObserving executes a git subcommand that is only ever asked a question,
// with the opportunistic index refresh that would otherwise be its one write
// turned off.
//
// Commands like `git status` write the refreshed index back to disk as a
// cache, taking .git/index.lock to do it. That is a fine trade in an
// interactive shell and a bad one for a daemon: gitloop asks the same question
// on every cycle, so it would repeatedly contend for the index lock with
// whatever the user is doing in the same checkout, to persist a cache nobody
// asked for. GIT_OPTIONAL_LOCKS=0 suppresses exactly the writes git itself
// classifies as optional — the answer is unchanged, only the caching of it
// goes away — and leaves the mandatory index writes of add, commit, and merge
// untouched.
func (r *Runner) runObserving(args ...string) (Result, error) {
	cmd := exec.Command(r.executablePath(), args...)
	return r.runCommand(cmd, args, "GIT_OPTIONAL_LOCKS=0")
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
	// WaitDelay can fire after a successful process exit when an unrelated
	// descendant inherited stdout or stderr. CommandContext can likewise
	// report ctx.Err when cancellation races with an exit 0. In both cases
	// Git's successful ProcessState remains authoritative.
	if runErr != nil && cmd.ProcessState != nil && cmd.ProcessState.Success() &&
		(errors.Is(runErr, exec.ErrWaitDelay) || cancellationStarted.Load()) {
		runErr = nil
	}
	// The direct git process can exit on SIGTERM before a transport child that
	// ignored the same signal. Once git itself is gone there is no useful
	// graceful work left for those descendants to perform, so terminate any
	// surviving members before disarming the delayed group kill.
	if cancellationStarted.Load() && cmd.Process != nil {
		_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
	}
	close(commandDone)
	if ctxErr := ctx.Err(); cancellationStarted.Load() && ctxErr != nil && runErr != nil {
		var gitErr *Error
		if errors.As(runErr, &gitErr) && !errors.Is(gitErr.Err, ctxErr) {
			// Multiple %w operands preserve errors.Is for both causes without
			// errors.Join's newline, which would corrupt tabular status output.
			gitErr.Err = fmt.Errorf("%w: %w", ctxErr, gitErr.Err)
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

func (r *Runner) runCommand(cmd *exec.Cmd, args []string, extraEnv ...string) (Result, error) {
	cmd.Dir = r.Dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, extraEnv...)

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
