package gitcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
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
	// Timeout bounds each individual git invocation. Zero means no timeout.
	Timeout time.Duration
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
	ctx := context.Background()
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, "git", args...)
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
