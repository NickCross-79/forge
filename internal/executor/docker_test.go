package executor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireDocker skips the test when no Docker daemon is reachable. Docker is an
// optional backend, so its absence must not turn into a test failure.
func requireDocker(t *testing.T) *DockerExecutor {
	t.Helper()
	e := NewDocker("", "alpine:3.20")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := e.Available(ctx); err != nil {
		t.Skipf("skipping Docker test: %v", err)
	}
	return e
}

// --- tests that need no daemon ---

func TestDockerRunArgs(t *testing.T) {
	e := &DockerExecutor{Binary: "docker", User: "1000:1000", ExtraArgs: []string{"--network=none"}}
	job := Job{
		Name:      "build",
		Commands:  []string{"echo hi"},
		Workspace: "/tmp/ws",
		Env:       map[string]string{"B": "2", "A": "1"},
	}
	args := e.runArgs(job, "node:20", "/tmp/ws", "forge-build-abc")
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"run --rm --interactive",
		"--name forge-build-abc",
		"--volume /tmp/ws:" + ContainerWorkspace,
		"--workdir " + ContainerWorkspace,
		"--user 1000:1000",
		"--network=none",
		"node:20 /bin/sh -s",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q:\n%s", want, joined)
		}
	}

	// Values must be forwarded by name, never as KEY=VALUE on the command line,
	// which any user on the host could read from `ps`.
	if !strings.Contains(joined, "--env A --env B") {
		t.Errorf("env should be forwarded by name in sorted order:\n%s", joined)
	}
	if strings.Contains(joined, "A=1") || strings.Contains(joined, "B=2") {
		t.Errorf("env values must not appear in argv:\n%s", joined)
	}
}

func TestDockerRunArgsWorkingDir(t *testing.T) {
	e := &DockerExecutor{Binary: "docker"}
	job := Job{Name: "j", Commands: []string{"pwd"}, Workspace: "/tmp/ws", WorkingDir: "sub/dir"}
	args := e.runArgs(job, "alpine", "/tmp/ws", "n")

	joined := strings.Join(args, " ")
	want := "--workdir " + ContainerWorkspace + "/sub/dir"
	if !strings.Contains(joined, want) {
		t.Errorf("args missing %q:\n%s", want, joined)
	}
}

func TestDockerRequiresImage(t *testing.T) {
	e := &DockerExecutor{Binary: "docker"}
	res := e.Run(context.Background(), Job{
		Name: "j", Commands: []string{"echo hi"}, Workspace: t.TempDir(),
	})
	if res.Success() {
		t.Fatal("docker executor ran without an image")
	}
	if !strings.Contains(res.Err.Error(), "requires an image") {
		t.Errorf("Err = %v, want an image requirement error", res.Err)
	}
}

func TestDockerRejectsAbsoluteWorkingDir(t *testing.T) {
	e := &DockerExecutor{Binary: "docker", DefaultImage: "alpine"}
	res := e.Run(context.Background(), Job{
		Name: "j", Commands: []string{"pwd"}, Workspace: t.TempDir(), WorkingDir: "/etc",
	})
	if res.Success() {
		t.Fatal("docker executor accepted an absolute working_dir")
	}
}

func TestDockerNoCommands(t *testing.T) {
	e := &DockerExecutor{Binary: "docker", DefaultImage: "alpine"}
	res := e.Run(context.Background(), Job{Name: "j", Workspace: t.TempDir()})
	if res.Success() || res.Err == nil {
		t.Fatal("expected a failure for a job with no commands")
	}
}

func TestDockerAvailableReportsMissingBinary(t *testing.T) {
	e := NewDocker("definitely-not-a-real-container-runtime", "alpine")
	err := e.Available(context.Background())
	if err == nil {
		t.Fatal("Available() returned nil for a missing binary")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("err = %v, want a 'not installed' message", err)
	}
}

func TestContainerNameIsSafeAndUnique(t *testing.T) {
	a, err := containerName("Build/Job Name!")
	if err != nil {
		t.Fatal(err)
	}
	b, err := containerName("Build/Job Name!")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("container names collide across calls")
	}
	if !strings.HasPrefix(a, "forge-build-job-name-") {
		t.Errorf("name = %q, want a sanitised forge- prefix", a)
	}
	for _, r := range a {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !valid {
			t.Errorf("name %q contains an invalid character %q", a, string(r))
		}
	}

	long, err := containerName(strings.Repeat("x", 200))
	if err != nil {
		t.Fatal(err)
	}
	if len(long) > 64 {
		t.Errorf("name length = %d, want it bounded", len(long))
	}
}

// --- tests that need a daemon ---

func TestDockerExecutorEndToEnd(t *testing.T) {
	e := requireDocker(t)
	ws := t.TempDir()
	var stdout, stderr bytes.Buffer

	res := e.Run(context.Background(), Job{
		Name:      "docker-e2e",
		Commands:  []string{"echo from-container", "echo to-stderr >&2", "echo data > out.txt"},
		Workspace: ws,
		Image:     "alpine:3.20",
		Env:       map[string]string{"GREETING": "hello"},
		Stdout:    &stdout,
		Stderr:    &stderr,
	})
	if !res.Success() {
		t.Fatalf("container job failed: %v\nstdout: %s\nstderr: %s", res.Err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "from-container") {
		t.Errorf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "to-stderr") {
		t.Errorf("stderr = %q", stderr.String())
	}
	// The workspace is bind-mounted, so a file written inside is visible outside.
	if _, err := os.Stat(filepath.Join(ws, "out.txt")); err != nil {
		t.Errorf("file written in the container is missing from the workspace: %v", err)
	}
}

func TestDockerExecutorEnvIsForwarded(t *testing.T) {
	e := requireDocker(t)
	var stdout bytes.Buffer
	res := e.Run(context.Background(), Job{
		Name:      "docker-env",
		Commands:  []string{"echo [$GREETING]"},
		Workspace: t.TempDir(),
		Image:     "alpine:3.20",
		Env:       map[string]string{"GREETING": "hello"},
		Stdout:    &stdout,
	})
	if !res.Success() {
		t.Fatalf("job failed: %v", res.Err)
	}
	if !strings.Contains(stdout.String(), "[hello]") {
		t.Errorf("env was not forwarded into the container: %q", stdout.String())
	}
}

func TestDockerExecutorExitCode(t *testing.T) {
	e := requireDocker(t)
	res := e.Run(context.Background(), Job{
		Name: "docker-fail", Commands: []string{"exit 7"},
		Workspace: t.TempDir(), Image: "alpine:3.20",
	})
	if res.Success() {
		t.Fatal("a container that exited 7 reported success")
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
}

func TestDockerExecutorTimeout(t *testing.T) {
	e := requireDocker(t)
	res := e.Run(context.Background(), Job{
		Name: "docker-timeout", Commands: []string{"sleep 60"},
		Workspace: t.TempDir(), Image: "alpine:3.20",
		Timeout: 2 * time.Second,
	})
	if res.Success() {
		t.Fatal("a container that exceeded its timeout reported success")
	}
	if !res.TimedOut {
		t.Errorf("TimedOut = false, want true (err = %v)", res.Err)
	}
}
