// Package executor runs a job's commands and reports the outcome.
//
// Two backends implement the same interface: LocalExecutor spawns child
// processes on the host, and DockerExecutor runs the job inside an ephemeral
// container. Nothing here depends on Docker being installed unless a job
// explicitly asks for it.
//
// # Security
//
// Commands are executed through a shell, deliberately: the pipeline format
// promises redirection, pipes and globbing, and those are shell features. A
// pipeline file is therefore executable content and must be trusted exactly as
// much as a shell script in the same repository. Local execution runs with the
// full OS permissions of the user who invoked forge. Docker execution is the
// isolation boundary when that matters.
package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Job is a unit of work handed to an executor. It is a flat, resolved
// description: the caller has already merged variables, applied defaults and
// decided on paths, so executors do no policy of their own.
type Job struct {
	// Name is used in log lines and error messages.
	Name string
	// Commands are run in order, in a single shell session, so `cd` and shell
	// variables set by one command are visible to the next.
	Commands []string
	// Env is the complete environment for the job. Executors do not inherit the
	// host environment implicitly; whatever should be visible must be in here.
	Env map[string]string
	// Workspace is the absolute directory the job runs in. For Docker it is
	// bind-mounted into the container.
	Workspace string
	// WorkingDir is an optional path relative to Workspace. It has already been
	// verified not to escape the workspace.
	WorkingDir string
	// Timeout bounds the whole job. Zero means no executor-imposed limit, though
	// the caller's context may still carry one.
	Timeout time.Duration
	// Image is the container image for Docker execution.
	Image string
	// Stdout and Stderr receive the job's output. They are captured separately so
	// diagnostics can be read apart from real output.
	Stdout io.Writer
	Stderr io.Writer
	// EchoCommands prints each command before running it, the way CI logs
	// normally do. Disabled in tests that assert on exact output.
	EchoCommands bool
}

// Result is the outcome of one attempt at a job.
type Result struct {
	// ExitCode is the shell's exit status. It is -1 when no process ran or when
	// the process was killed before it could report one.
	ExitCode int
	// Err is set when the job did not complete successfully. A non-zero exit
	// status is reported as an error here as well as in ExitCode.
	Err error
	// TimedOut reports that the job exceeded its timeout and was killed.
	TimedOut bool
	// Cancelled reports that the caller's context was cancelled.
	Cancelled bool
	StartedAt time.Time
	EndedAt   time.Time
}

// Success reports whether the job completed with a zero exit status.
func (r Result) Success() bool { return r.Err == nil && r.ExitCode == 0 }

// Duration is how long the attempt took.
func (r Result) Duration() time.Duration { return r.EndedAt.Sub(r.StartedAt) }

// Executor runs a job to completion and reports the result. Implementations must
// honour context cancellation promptly and must never panic on malformed input.
type Executor interface {
	Run(ctx context.Context, job Job) Result
}

// ErrNoCommands is returned when a job has nothing to run.
var ErrNoCommands = errors.New("job has no commands")

// EnvSlice renders an environment map as sorted KEY=VALUE pairs. Sorting keeps
// command lines and env files reproducible, which makes failures easier to
// compare between runs.
func EnvSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// failure builds a Result for an error that happened before or around execution.
func failure(started time.Time, exitCode int, err error) Result {
	return Result{ExitCode: exitCode, Err: err, StartedAt: started, EndedAt: time.Now()}
}

// classify turns a context error into the right Result flags. A timeout and a
// user cancellation both kill the process, but they mean different things to the
// scheduler: a timeout is a job failure worth retrying, a cancellation is not.
func classify(ctx context.Context, runCtx context.Context, res *Result) {
	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		res.TimedOut = true
	case ctx.Err() != nil:
		res.Cancelled = true
	}
}

// shellQuote renders s as a single-quoted POSIX shell word, so that echoing a
// command back to the log cannot itself be interpreted by the shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildScript turns a job's commands into one shell script.
//
// Running the commands as a single script rather than one process each is what
// makes `cd dist` affect the following command, matching how a developer would
// run the same steps by hand. `set -e` stops at the first failure so that a
// broken step cannot be masked by a later successful one.
func buildScript(job Job) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	for _, cmd := range job.Commands {
		if job.EchoCommands {
			// printf, not echo: echo's handling of backslashes varies between shells.
			fmt.Fprintf(&b, "printf '%%s\\n' %s\n", shellQuote("$ "+cmd))
		}
		b.WriteString(cmd)
		b.WriteByte('\n')
	}
	return b.String()
}
