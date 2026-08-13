package executor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// ContainerWorkspace is where the job workspace is mounted inside the container.
const ContainerWorkspace = "/forge/workspace"

// DockerExecutor runs a job inside an ephemeral container.
//
// This is the isolation boundary: the job sees only its own workspace, only the
// environment variables it was given, and only the container's filesystem. The
// container is removed when the job finishes, so nothing accumulates between runs.
type DockerExecutor struct {
	// Binary is the docker CLI to invoke. Defaults to "docker", so a drop-in
	// replacement such as podman can be used by name.
	Binary string
	// DefaultImage is used by jobs that select the docker executor without
	// naming an image.
	DefaultImage string
	// ExtraArgs are appended to every `docker run` invocation, for things like
	// `--network=none` or a memory cap.
	ExtraArgs []string
	// User overrides the uid:gid the container runs as. Empty means the invoking
	// user, so files written into the mounted workspace are not left root-owned.
	User string
}

// NewDocker returns a DockerExecutor.
func NewDocker(binary, defaultImage string) *DockerExecutor {
	if binary == "" {
		binary = "docker"
	}
	return &DockerExecutor{Binary: binary, DefaultImage: defaultImage}
}

// Available reports whether a working Docker daemon can be reached. It is used
// to fail early with a clear message, and by tests to skip when there is no
// daemon rather than reporting a spurious failure.
func (e *DockerExecutor) Available(ctx context.Context) error {
	binary := e.binary()
	if _, err := exec.LookPath(binary); err != nil {
		return fmt.Errorf("%s is not installed or not on PATH: %w", binary, err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(probeCtx, binary, "info", "--format", "{{.ServerVersion}}") //nolint:gosec // binary is operator-configured
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cannot reach the Docker daemon via %s: %w: %s",
			binary, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (e *DockerExecutor) binary() string {
	if e.Binary == "" {
		return "docker"
	}
	return e.Binary
}

// Run implements Executor.
func (e *DockerExecutor) Run(ctx context.Context, job Job) Result {
	started := time.Now()

	if len(job.Commands) == 0 {
		return failure(started, -1, ErrNoCommands)
	}
	image := job.Image
	if image == "" {
		image = e.DefaultImage
	}
	if image == "" {
		return failure(started, -1, errors.New("docker executor requires an image"))
	}
	workspace, err := filepath.Abs(job.Workspace)
	if err != nil {
		return failure(started, -1, fmt.Errorf("resolve workspace: %w", err))
	}
	if st, err := os.Stat(workspace); err != nil || !st.IsDir() {
		return failure(started, -1, fmt.Errorf("workspace %s is not a directory", workspace))
	}
	if job.WorkingDir != "" && filepath.IsAbs(job.WorkingDir) {
		return failure(started, -1, fmt.Errorf("working_dir %q must be relative to the workspace", job.WorkingDir))
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if job.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, job.Timeout)
		defer cancel()
	}

	name, err := containerName(job.Name)
	if err != nil {
		return failure(started, -1, err)
	}

	args := e.runArgs(job, image, workspace, name)

	// The script goes in on stdin, so a job's commands never appear in the
	// container's argv or in `docker ps` output.
	cmd := exec.Command(e.binary(), args...) //nolint:gosec // running pipeline commands is the point
	cmd.Stdin = strings.NewReader(buildScript(job))
	cmd.Stdout = job.Stdout
	cmd.Stderr = job.Stderr
	// Values are handed to the docker CLI through its own environment and
	// forwarded with `-e KEY`, rather than as `-e KEY=VALUE` arguments which
	// would be readable by any user on the host via `ps`.
	cmd.Env = append(os.Environ(), EnvSlice(job.Env)...)

	if err := cmd.Start(); err != nil {
		return failure(started, -1, fmt.Errorf("start docker: %w", err))
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-runCtx.Done():
		// Killing the docker CLI does not stop the container it started, so the
		// container has to be stopped by name.
		e.stopContainer(name)
		select {
		case waitErr = <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
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
		res.Err = fmt.Errorf("job %q failed in container %s: exit status %d", job.Name, image, res.ExitCode)
	}
	return res
}

// runArgs builds the `docker run` command line.
func (e *DockerExecutor) runArgs(job Job, image, workspace, name string) []string {
	workdir := ContainerWorkspace
	if job.WorkingDir != "" {
		workdir = filepath.ToSlash(filepath.Join(ContainerWorkspace, filepath.Clean(job.WorkingDir)))
	}

	args := []string{
		"run",
		"--rm",
		"--interactive",
		"--name", name,
		"--volume", workspace + ":" + ContainerWorkspace,
		"--workdir", workdir,
	}

	if user := e.userFlag(); user != "" {
		args = append(args, "--user", user)
	}

	// Forward only the job's own variables. `-e KEY` takes the value from the
	// docker CLI's environment, which is set on the command above.
	keys := make([]string, 0, len(job.Env))
	for k := range job.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--env", k)
	}

	args = append(args, e.ExtraArgs...)
	args = append(args, image, "/bin/sh", "-s")
	return args
}

// userFlag returns the uid:gid the container should run as. Running as the
// invoking user keeps files created in the mounted workspace owned by that user
// instead of by root, which would otherwise make artifacts and cleanup painful.
func (e *DockerExecutor) userFlag() string {
	if e.User != "" {
		return e.User
	}
	if runtime.GOOS == "windows" {
		return ""
	}
	uid, gid := os.Getuid(), os.Getgid()
	if uid < 0 || gid < 0 {
		return ""
	}
	return fmt.Sprintf("%d:%d", uid, gid)
}

// stopContainer stops a running container by name, ignoring the error when it
// has already exited.
func (e *DockerExecutor) stopContainer(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.binary(), "kill", name) //nolint:gosec // binary is operator-configured
	_ = cmd.Run()
}

// containerName builds a unique, docker-legal container name that identifies the
// job it belongs to, so a stray container can be traced back to its source.
func containerName(jobName string) (string, error) {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate container name: %w", err)
	}
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, jobName)
	if len(safe) > 32 {
		safe = safe[:32]
	}
	if safe == "" {
		safe = "job"
	}
	return fmt.Sprintf("forge-%s-%s", safe, hex.EncodeToString(buf[:])), nil
}
