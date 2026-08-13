package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// LocalExecutor runs a job's commands as child processes on the host.
//
// It runs with the invoking user's OS permissions — there is no sandbox. What it
// does provide is containment of the obvious accidents: the job starts in its own
// workspace, sees only the environment it was given, is bounded by a timeout, and
// is killed as a process group so that background children cannot outlive it.
type LocalExecutor struct {
	// Shell overrides the interpreter. Defaults to /bin/sh (cmd.exe on Windows).
	Shell string
}

// NewLocal returns a LocalExecutor using the given shell, or the platform default
// when shell is empty.
func NewLocal(shell string) *LocalExecutor { return &LocalExecutor{Shell: shell} }

// Run implements Executor.
func (e *LocalExecutor) Run(ctx context.Context, job Job) Result {
	started := time.Now()

	if len(job.Commands) == 0 {
		return failure(started, -1, ErrNoCommands)
	}

	workdir, err := resolveWorkdir(job)
	if err != nil {
		return failure(started, -1, err)
	}

	// A separate context carries the timeout so a deadline can be told apart from
	// a caller-initiated cancellation when the process dies.
	runCtx := ctx
	var cancel context.CancelFunc
	if job.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, job.Timeout)
		defer cancel()
	}

	script := buildScript(job)
	shell, args := e.shellCommand()

	// The script arrives on stdin rather than as an argument: command lines are
	// visible to every user on the host via `ps`, and a job's commands may embed
	// values that should not be.
	cmd := exec.Command(shell, args...) //nolint:gosec // executing pipeline commands is the point
	cmd.Dir = workdir
	cmd.Env = EnvSlice(job.Env)
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout = job.Stdout
	cmd.Stderr = job.Stderr
	configureProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		return failure(started, -1, fmt.Errorf("start %s: %w", shell, err))
	}

	// Kill the whole process group rather than just the shell, so that anything
	// the job backgrounded dies with it instead of leaking.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-runCtx.Done():
		terminateGroup(cmd)
		// Give the group a moment to exit on the signal before giving up on it.
		select {
		case waitErr = <-done:
		case <-time.After(5 * time.Second):
			killGroup(cmd)
			waitErr = <-done
		}
	}

	res := Result{StartedAt: started, EndedAt: time.Now()}
	classify(ctx, runCtx, &res)

	switch {
	case waitErr == nil && !res.TimedOut && !res.Cancelled:
		res.ExitCode = 0
	case res.TimedOut:
		res.ExitCode = exitCodeOf(waitErr)
		res.Err = fmt.Errorf("job %q timed out after %s", job.Name, job.Timeout)
	case res.Cancelled:
		res.ExitCode = exitCodeOf(waitErr)
		res.Err = fmt.Errorf("job %q was cancelled", job.Name)
	default:
		res.ExitCode = exitCodeOf(waitErr)
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			res.Err = fmt.Errorf("job %q failed: exit status %d", job.Name, res.ExitCode)
		} else {
			res.Err = fmt.Errorf("job %q failed: %w", job.Name, waitErr)
		}
	}
	return res
}

// shellCommand returns the interpreter and the arguments that make it read a
// script from stdin.
func (e *LocalExecutor) shellCommand() (string, []string) {
	if e.Shell != "" {
		if runtime.GOOS == "windows" {
			return e.Shell, nil
		}
		return e.Shell, []string{"-s"}
	}
	if runtime.GOOS == "windows" {
		// cmd.exe reads a batch script from stdin when given no /c argument.
		return "cmd.exe", nil
	}
	return "/bin/sh", []string{"-s"}
}

// resolveWorkdir returns the absolute directory the job runs in, after checking
// that a job-specified working_dir stays inside the workspace.
//
// The check is done on the resolved path — after symlinks — because a symlink
// inside the workspace pointing outside it would otherwise be a way around the
// restriction.
func resolveWorkdir(job Job) (string, error) {
	if job.Workspace == "" {
		return "", errors.New("job workspace is required")
	}
	base, err := filepath.Abs(job.Workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		return "", fmt.Errorf("workspace %s is not a directory", base)
	}
	if job.WorkingDir == "" {
		return base, nil
	}
	if filepath.IsAbs(job.WorkingDir) {
		return "", fmt.Errorf("working_dir %q must be relative to the workspace", job.WorkingDir)
	}

	target := filepath.Join(base, job.WorkingDir)
	if err := os.MkdirAll(target, 0o750); err != nil {
		return "", fmt.Errorf("create working_dir %q: %w", job.WorkingDir, err)
	}

	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("resolve working_dir: %w", err)
	}
	if !withinDir(realBase, realTarget) {
		return "", fmt.Errorf("working_dir %q escapes the workspace", job.WorkingDir)
	}
	return realTarget, nil
}

// withinDir reports whether target is base or lives beneath it. Both paths must
// already be absolute and symlink-resolved.
func withinDir(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// exitCodeOf extracts a process exit status, returning -1 when there is none.
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
