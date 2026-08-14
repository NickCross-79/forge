package executor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func newJob(t *testing.T, commands ...string) (Job, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	return Job{
		Name:      "test",
		Commands:  commands,
		Workspace: t.TempDir(),
		Env:       map[string]string{"PATH": os.Getenv("PATH")},
		Stdout:    &stdout,
		Stderr:    &stderr,
	}, &stdout, &stderr
}

func TestLocalExecutorSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell semantics")
	}
	job, stdout, stderr := newJob(t, "echo hello", "echo world")
	res := NewLocal("").Run(context.Background(), job)

	if !res.Success() {
		t.Fatalf("Success() = false, err = %v, exit = %d", res.Err, res.ExitCode)
	}
	if got := stdout.String(); got != "hello\nworld\n" {
		t.Errorf("stdout = %q, want %q", got, "hello\nworld\n")
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
	if res.Duration() < 0 {
		t.Error("Duration() is negative")
	}
}

func TestLocalExecutorSeparatesStreams(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell semantics")
	}
	job, stdout, stderr := newJob(t, "echo to-stdout", "echo to-stderr >&2")
	res := NewLocal("").Run(context.Background(), job)

	if !res.Success() {
		t.Fatalf("job failed: %v", res.Err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "to-stdout" {
		t.Errorf("stdout = %q, want to-stdout", got)
	}
	if got := strings.TrimSpace(stderr.String()); got != "to-stderr" {
		t.Errorf("stderr = %q, want to-stderr", got)
	}
}

func TestLocalExecutorExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell semantics")
	}
	job, _, _ := newJob(t, "exit 42")
	res := NewLocal("").Run(context.Background(), job)

	if res.Success() {
		t.Fatal("Success() = true for a job that exited 42")
	}
	if res.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", res.ExitCode)
	}
	if res.Err == nil {
		t.Error("Err is nil for a failed job")
	}
	if res.TimedOut || res.Cancelled {
		t.Error("a plain failure should not be flagged as a timeout or cancellation")
	}
}

func TestLocalExecutorStopsAtFirstFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell semantics")
	}
	job, stdout, _ := newJob(t, "echo first", "exit 3", "echo unreachable")
	res := NewLocal("").Run(context.Background(), job)

	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if strings.Contains(stdout.String(), "unreachable") {
		t.Errorf("commands after a failure should not run; stdout = %q", stdout.String())
	}
}

func TestLocalExecutorSharesShellSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell semantics")
	}
	// All commands run in one shell, so `cd` and shell variables persist.
	job, stdout, _ := newJob(t, "mkdir -p sub", "cd sub", "VAR=kept", "pwd", "echo $VAR")
	res := NewLocal("").Run(context.Background(), job)
	if !res.Success() {
		t.Fatalf("job failed: %v", res.Err)
	}
	out := stdout.String()
	if !strings.Contains(out, "sub") {
		t.Errorf("cd did not persist between commands; stdout = %q", out)
	}
	if !strings.Contains(out, "kept") {
		t.Errorf("shell variables did not persist between commands; stdout = %q", out)
	}
}

func TestLocalExecutorEnvironmentIsExplicit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell semantics")
	}
	t.Setenv("FORGE_TEST_HOST_SECRET", "leaked")

	job, stdout, _ := newJob(t, "echo [$FORGE_TEST_HOST_SECRET]", "echo [$JOB_VAR]")
	job.Env["JOB_VAR"] = "provided"
	res := NewLocal("").Run(context.Background(), job)
	if !res.Success() {
		t.Fatalf("job failed: %v", res.Err)
	}
	out := stdout.String()
	if strings.Contains(out, "leaked") {
		t.Errorf("host environment leaked into the job: %q", out)
	}
	if !strings.Contains(out, "provided") {
		t.Errorf("job environment was not applied: %q", out)
	}
}

func TestLocalExecutorWorkspaceIsCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell semantics")
	}
	job, stdout, _ := newJob(t, "pwd")
	res := NewLocal("").Run(context.Background(), job)
	if !res.Success() {
		t.Fatalf("job failed: %v", res.Err)
	}
	want, err := filepath.EvalSymlinks(job.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(stdout.String()); got != want {
		t.Errorf("cwd = %q, want %q", got, want)
	}
}

func TestLocalExecutorWorkingDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell semantics")
	}
	job, stdout, _ := newJob(t, "pwd")
	job.WorkingDir = "nested/dir"
	res := NewLocal("").Run(context.Background(), job)
	if !res.Success() {
		t.Fatalf("job failed: %v", res.Err)
	}
	if !strings.Contains(stdout.String(), filepath.Join("nested", "dir")) {
		t.Errorf("working_dir was not applied; pwd = %q", stdout.String())
	}
}

func TestLocalExecutorRejectsEscapingWorkingDir(t *testing.T) {
	tests := []struct {
		name       string
		workingDir string
	}{
		{"parent traversal", "../escape"},
		{"deep traversal", "a/../../escape"},
		{"absolute", "/etc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job, _, _ := newJob(t, "pwd")
			job.WorkingDir = tc.workingDir
			res := NewLocal("").Run(context.Background(), job)
			if res.Success() {
				t.Fatal("job succeeded with a working_dir outside the workspace")
			}
			if res.Err == nil {
				t.Fatal("Err is nil")
			}
		})
	}
}

func TestLocalExecutorRejectsSymlinkedWorkingDirEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	job, _, _ := newJob(t, "pwd")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(job.Workspace, "link")); err != nil {
		t.Skipf("cannot create symlinks: %v", err)
	}
	job.WorkingDir = "link"

	res := NewLocal("").Run(context.Background(), job)
	if res.Success() {
		t.Fatal("a symlinked working_dir pointing outside the workspace was accepted")
	}
	if !strings.Contains(res.Err.Error(), "escapes the workspace") {
		t.Errorf("Err = %v, want an escape error", res.Err)
	}
}

func TestLocalExecutorTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell semantics")
	}
	job, _, _ := newJob(t, "sleep 30")
	job.Timeout = 150 * time.Millisecond

	start := time.Now()
	res := NewLocal("").Run(context.Background(), job)
	elapsed := time.Since(start)

	if res.Success() {
		t.Fatal("a job that exceeded its timeout reported success")
	}
	if !res.TimedOut {
		t.Errorf("TimedOut = false, want true (err = %v)", res.Err)
	}
	if res.Cancelled {
		t.Error("Cancelled = true, want false: the deadline fired, not the caller")
	}
	if elapsed > 10*time.Second {
		t.Errorf("timeout took %v to take effect", elapsed)
	}
	if !strings.Contains(res.Err.Error(), "timed out") {
		t.Errorf("Err = %v, want a timeout message", res.Err)
	}
}

func TestLocalExecutorCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell semantics")
	}
	job, _, _ := newJob(t, "sleep 30")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan Result, 1)
	go func() { done <- NewLocal("").Run(ctx, job) }()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case res := <-done:
		if res.Success() {
			t.Fatal("a cancelled job reported success")
		}
		if !res.Cancelled {
			t.Errorf("Cancelled = false, want true (err = %v)", res.Err)
		}
		if res.TimedOut {
			t.Error("TimedOut = true, want false: the caller cancelled")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancellation did not stop the job")
	}
}

func TestLocalExecutorKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are POSIX-only")
	}
	// The job backgrounds a child that writes to a file after a delay. If the
	// process group is killed correctly, that file is never created.
	job, _, _ := newJob(t, "sh -c 'sleep 3; echo orphan > survivor.txt' & sleep 30")
	marker := filepath.Join(job.Workspace, "survivor.txt")
	job.Timeout = 200 * time.Millisecond

	res := NewLocal("").Run(context.Background(), job)
	if res.Success() {
		t.Fatal("job should have timed out")
	}

	// Wait past the point at which the orphan would have written.
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a backgrounded child outlived the job: the process group was not killed")
	}
}

func TestLocalExecutorNoCommands(t *testing.T) {
	job, _, _ := newJob(t)
	res := NewLocal("").Run(context.Background(), job)
	if res.Success() {
		t.Fatal("a job with no commands reported success")
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "no commands") {
		t.Errorf("Err = %v, want ErrNoCommands", res.Err)
	}
}

func TestLocalExecutorMissingWorkspace(t *testing.T) {
	job, _, _ := newJob(t, "echo hi")
	job.Workspace = filepath.Join(t.TempDir(), "does-not-exist")
	res := NewLocal("").Run(context.Background(), job)
	if res.Success() {
		t.Fatal("job ran with a missing workspace")
	}
}

func TestLocalExecutorEchoCommands(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell semantics")
	}
	job, stdout, _ := newJob(t, `echo "it's here"`)
	job.EchoCommands = true
	res := NewLocal("").Run(context.Background(), job)
	if !res.Success() {
		t.Fatalf("job failed: %v", res.Err)
	}
	out := stdout.String()
	// The echoed command must appear verbatim, quotes and all, and must not be
	// re-interpreted by the shell.
	if !strings.Contains(out, `$ echo "it's here"`) {
		t.Errorf("command echo missing or mangled; stdout = %q", out)
	}
	if !strings.Contains(out, "it's here") {
		t.Errorf("command output missing; stdout = %q", out)
	}
}

func TestLocalExecutorConcurrentJobs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell semantics")
	}
	// The executor holds no per-run state, so one instance must be safe to share.
	exec := NewLocal("")
	var wg sync.WaitGroup
	results := make([]Result, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			job, _, _ := newJob(t, "echo concurrent")
			results[i] = exec.Run(context.Background(), job)
		}(i)
	}
	wg.Wait()
	for i, res := range results {
		if !res.Success() {
			t.Errorf("job %d failed: %v", i, res.Err)
		}
	}
}

func TestEnvSliceIsSorted(t *testing.T) {
	got := EnvSlice(map[string]string{"B": "2", "A": "1", "C": "3"})
	want := []string{"A=1", "B=2", "C=3"}
	if len(got) != len(want) {
		t.Fatalf("EnvSlice() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("EnvSlice() = %v, want %v", got, want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"simple", "'simple'"},
		{"with space", "'with space'"},
		{"it's", `'it'\''s'`},
		{`$(rm -rf /)`, `'$(rm -rf /)'`},
	}
	for _, tc := range tests {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildScriptFailsFast(t *testing.T) {
	script := buildScript(Job{Commands: []string{"a", "b"}})
	if !strings.HasPrefix(script, "set -e\n") {
		t.Errorf("script does not enable errexit: %q", script)
	}
	if !strings.Contains(script, "a\nb\n") {
		t.Errorf("script does not contain the commands in order: %q", script)
	}
}

func TestWithinDir(t *testing.T) {
	tests := []struct {
		base, target string
		want         bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/b/c", true},
		{"/a/b", "/a/b/c/d", true},
		{"/a/b", "/a/c", false},
		{"/a/b", "/a", false},
		{"/a/b", "/a/bc", false}, // prefix match must not be mistaken for containment
	}
	for _, tc := range tests {
		if got := withinDir(tc.base, tc.target); got != tc.want {
			t.Errorf("withinDir(%q, %q) = %v, want %v", tc.base, tc.target, got, tc.want)
		}
	}
}
