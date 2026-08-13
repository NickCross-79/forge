package artifacts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, rel, content string) string {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return full
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func collectedPaths(files []File) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

// --- path safety ------------------------------------------------------------

func TestSafeJoinRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	hostile := []string{
		"../escape",
		"../../etc/passwd",
		"a/../../escape",
		"a/b/../../../escape",
		"./../escape",
		"/etc/passwd",
		"/",
	}
	for _, rel := range hostile {
		t.Run(rel, func(t *testing.T) {
			if _, err := SafeJoin(base, rel); !errors.Is(err, ErrPathEscape) {
				t.Errorf("SafeJoin(%q) error = %v, want ErrPathEscape", rel, err)
			}
		})
	}
}

func TestSafeJoinAllowsInnocentPaths(t *testing.T) {
	base := t.TempDir()
	fine := []string{"file.txt", "dist/output.txt", "a/b/c/d.txt", "./dist/x", "a/../b"}
	for _, rel := range fine {
		t.Run(rel, func(t *testing.T) {
			got, err := SafeJoin(base, rel)
			if err != nil {
				t.Fatalf("SafeJoin(%q) error = %v", rel, err)
			}
			if !Contains(filepath.Clean(base), got) {
				t.Errorf("SafeJoin(%q) = %q, which is outside %q", rel, got, base)
			}
		})
	}
}

func TestSafeResolveRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	base := t.TempDir()
	outside := t.TempDir()
	writeFile(t, outside, "secret.txt", "top secret")

	if err := os.Symlink(outside, filepath.Join(base, "link")); err != nil {
		t.Skipf("cannot create symlinks: %v", err)
	}
	// Lexically this stays inside base, so only symlink resolution catches it.
	if _, err := SafeResolve(base, "link/secret.txt"); !errors.Is(err, ErrPathEscape) {
		t.Errorf("SafeResolve through an escaping symlink: error = %v, want ErrPathEscape", err)
	}

	// A symlink that stays inside the base is fine.
	writeFile(t, base, "real/file.txt", "ok")
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "inner")); err != nil {
		t.Fatal(err)
	}
	if _, err := SafeResolve(base, "inner/file.txt"); err != nil {
		t.Errorf("SafeResolve on an internal symlink: error = %v, want nil", err)
	}
}

func TestSafeResolveAllowsNonExistentDestination(t *testing.T) {
	base := t.TempDir()
	// Restoring an artifact writes to a path that does not exist yet.
	if _, err := SafeResolve(base, "does/not/exist/yet.txt"); err != nil {
		t.Errorf("SafeResolve() error = %v, want nil for a new destination", err)
	}
	if _, err := SafeResolve(base, "../outside.txt"); !errors.Is(err, ErrPathEscape) {
		t.Errorf("SafeResolve(../outside.txt) error = %v, want ErrPathEscape", err)
	}
}

func TestContains(t *testing.T) {
	tests := []struct {
		base, target string
		want         bool
	}{
		{"/a", "/a", true},
		{"/a", "/a/b", true},
		{"/a", "/ab", false},
		{"/a", "/", false},
		{"/a/b", "/a", false},
	}
	for _, tc := range tests {
		if got := Contains(tc.base, tc.target); got != tc.want {
			t.Errorf("Contains(%q, %q) = %v, want %v", tc.base, tc.target, got, tc.want)
		}
	}
}

func TestNormalizePattern(t *testing.T) {
	tests := []struct{ in, want string }{
		{"dist/", "dist/**"},
		{"./dist/x", "dist/x"},
		{"  dist/*.txt  ", "dist/*.txt"},
		{"**/*.log", "**/*.log"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := NormalizePattern(tc.in); got != tc.want {
			t.Errorf("NormalizePattern(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- collection -------------------------------------------------------------

func TestCollectDirectoryShorthand(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "dist/output.txt", "hello\n")
	writeFile(t, ws, "dist/nested/deep.txt", "deep\n")
	writeFile(t, ws, "src/main.go", "package main")

	s := newStore(t)
	// `dist/` and `dist` must behave identically.
	for _, pattern := range []string{"dist/", "dist"} {
		files, err := s.Collect(context.Background(), CollectRequest{
			RunID: 1, JobName: "build", Workspace: ws, Paths: []string{pattern},
		})
		if err != nil {
			t.Fatalf("Collect(%q) error = %v", pattern, err)
		}
		got := collectedPaths(files)
		want := []string{"dist/nested/deep.txt", "dist/output.txt"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("Collect(%q) = %v, want %v", pattern, got, want)
		}
	}
}

func TestCollectGlobPatterns(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "a.txt", "a")
	writeFile(t, ws, "b.log", "b")
	writeFile(t, ws, "deep/nested/c.txt", "c")
	writeFile(t, ws, "deep/nested/d.log", "d")

	s := newStore(t)
	tests := []struct {
		name    string
		paths   []string
		exclude []string
		want    []string
	}{
		{"single glob", []string{"*.txt"}, nil, []string{"a.txt"}},
		{"recursive glob", []string{"**/*.txt"}, nil, []string{"a.txt", "deep/nested/c.txt"}},
		{"multiple patterns", []string{"*.txt", "*.log"}, nil, []string{"a.txt", "b.log"}},
		{"exclude", []string{"**/*"}, []string{"**/*.log"}, []string{"a.txt", "deep/nested/c.txt"}},
		{"exclude subtree", []string{"**/*"}, []string{"deep/**"}, []string{"a.txt", "b.log"}},
		{"no matches", []string{"nothing/*"}, nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			files, err := s.Collect(context.Background(), CollectRequest{
				RunID: 1, JobName: tc.name, Workspace: ws, Paths: tc.paths, Exclude: tc.exclude,
			})
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			got := strings.Join(collectedPaths(files), ",")
			want := strings.Join(tc.want, ",")
			if got != want {
				t.Errorf("Collect() = [%s], want [%s]", got, want)
			}
		})
	}
}

func TestCollectRecordsMetadata(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "dist/output.txt", "hello\n")

	s := newStore(t)
	files, err := s.Collect(context.Background(), CollectRequest{
		RunID: 42, JobName: "build", Workspace: ws, Paths: []string{"dist/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("len(files) = %d, want 1", len(files))
	}
	f := files[0]
	if f.Size != 6 {
		t.Errorf("Size = %d, want 6", f.Size)
	}
	// sha256 of "hello\n"
	const want = "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
	if f.SHA256 != want {
		t.Errorf("SHA256 = %q, want %q", f.SHA256, want)
	}
	if f.StorePath != "42/build/dist/output.txt" {
		t.Errorf("StorePath = %q, want 42/build/dist/output.txt", f.StorePath)
	}

	// The bytes must actually be in the store, outside the workspace.
	body, err := os.ReadFile(filepath.Join(s.Root(), filepath.FromSlash(f.StorePath)))
	if err != nil {
		t.Fatalf("stored artifact is missing: %v", err)
	}
	if string(body) != "hello\n" {
		t.Errorf("stored content = %q, want %q", body, "hello\n")
	}
}

func TestCollectRefusesToEscapeWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	ws := t.TempDir()
	outside := t.TempDir()
	writeFile(t, outside, "secret.txt", "top secret")
	writeFile(t, ws, "safe.txt", "fine")

	// A job could create this symlink itself during execution.
	if err := os.Symlink(outside, filepath.Join(ws, "escape")); err != nil {
		t.Skipf("cannot create symlinks: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(ws, "direct.txt")); err != nil {
		t.Fatal(err)
	}

	s := newStore(t)
	files, err := s.Collect(context.Background(), CollectRequest{
		RunID: 1, JobName: "build", Workspace: ws, Paths: []string{"**/*"},
	})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	for _, f := range files {
		if strings.Contains(f.Path, "escape") || f.Path == "direct.txt" {
			t.Errorf("collected %q, which resolves outside the workspace", f.Path)
		}
	}
	// The legitimate file is still collected.
	if len(files) != 1 || files[0].Path != "safe.txt" {
		t.Errorf("Collect() = %v, want only safe.txt", collectedPaths(files))
	}

	// No stored artifact may contain the secret.
	err = filepath.Walk(s.Root(), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		body, err := os.ReadFile(path) //nolint:gosec // test-controlled path
		if err != nil {
			return err
		}
		if strings.Contains(string(body), "top secret") {
			t.Errorf("a file outside the workspace was collected into %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCollectPatternCannotEscape(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "inside.txt", "in")
	parent := filepath.Dir(ws)
	writeFile(t, parent, "sibling-secret.txt", "secret")

	s := newStore(t)
	// Matching is rooted at the workspace, so these patterns can never reach out.
	for _, pattern := range []string{"../*", "../../*", "/etc/*", "**/../*"} {
		files, err := s.Collect(context.Background(), CollectRequest{
			RunID: 1, JobName: "j", Workspace: ws, Paths: []string{pattern},
		})
		if err != nil {
			continue // an invalid pattern is an acceptable outcome too
		}
		for _, f := range files {
			if strings.Contains(f.Path, "secret") || strings.HasPrefix(f.Path, "..") {
				t.Errorf("pattern %q collected %q from outside the workspace", pattern, f.Path)
			}
		}
	}
}

func TestCollectSkipsDirectoriesAndIrregularFiles(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "emptydir"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, ws, "file.txt", "x")

	s := newStore(t)
	files, err := s.Collect(context.Background(), CollectRequest{
		RunID: 1, JobName: "j", Workspace: ws, Paths: []string{"**/*"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "file.txt" {
		t.Errorf("Collect() = %v, want only file.txt", collectedPaths(files))
	}
}

func TestCollectEnforcesSizeLimits(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "big.bin", strings.Repeat("x", 1024))
	writeFile(t, ws, "small.txt", "ok")

	s := newStore(t)
	s.MaxFileSize = 512
	_, err := s.Collect(context.Background(), CollectRequest{
		RunID: 1, JobName: "j", Workspace: ws, Paths: []string{"big.bin"},
	})
	if err == nil || !strings.Contains(err.Error(), "per-file limit") {
		t.Errorf("error = %v, want a per-file size limit error", err)
	}

	s2 := newStore(t)
	s2.MaxTotalSize = 512
	_, err = s2.Collect(context.Background(), CollectRequest{
		RunID: 1, JobName: "j", Workspace: ws, Paths: []string{"**/*"},
	})
	if err == nil || !strings.Contains(err.Error(), "per-job limit") {
		t.Errorf("error = %v, want a per-job size limit error", err)
	}
}

func TestCollectHonoursContextCancellation(t *testing.T) {
	ws := t.TempDir()
	for i := 0; i < 20; i++ {
		writeFile(t, ws, filepath.Join("many", string(rune('a'+i))+".txt"), "x")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := newStore(t)
	_, err := s.Collect(ctx, CollectRequest{
		RunID: 1, JobName: "j", Workspace: ws, Paths: []string{"**/*"},
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Collect() error = %v, want context.Canceled", err)
	}
}

func TestCollectSanitisesJobNameInStorePath(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "file.txt", "x")

	s := newStore(t)
	files, err := s.Collect(context.Background(), CollectRequest{
		RunID: 1, JobName: "../../evil", Workspace: ws, Paths: []string{"file.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatal("expected one artifact")
	}
	if strings.Contains(files[0].StorePath, "..") {
		t.Errorf("StorePath = %q, want the job name sanitised", files[0].StorePath)
	}
	abs := filepath.Join(s.Root(), filepath.FromSlash(files[0].StorePath))
	if !Contains(s.Root(), abs) {
		t.Errorf("artifact stored outside the store root: %q", abs)
	}
}

// --- retrieval and restore --------------------------------------------------

func TestOpenAndStat(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "dist/output.txt", "hello\n")
	s := newStore(t)
	files, err := s.Collect(context.Background(), CollectRequest{
		RunID: 1, JobName: "build", Workspace: ws, Paths: []string{"dist/"},
	})
	if err != nil {
		t.Fatal(err)
	}

	rc, err := s.Open(files[0].StorePath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = rc.Close() }()
	buf := make([]byte, 16)
	n, _ := rc.Read(buf)
	if string(buf[:n]) != "hello\n" {
		t.Errorf("Open() content = %q, want %q", buf[:n], "hello\n")
	}

	info, err := s.Stat(files[0].StorePath)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Size() != 6 {
		t.Errorf("Stat().Size() = %d, want 6", info.Size())
	}
}

func TestOpenRejectsTraversal(t *testing.T) {
	s := newStore(t)
	for _, p := range []string{"../../etc/passwd", "/etc/passwd", "1/../../../etc/passwd"} {
		if _, err := s.Open(p); !errors.Is(err, ErrPathEscape) {
			t.Errorf("Open(%q) error = %v, want ErrPathEscape", p, err)
		}
	}
}

func TestRestoreIntoWorkspace(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "dist/output.txt", "artifact body\n")
	s := newStore(t)
	files, err := s.Collect(context.Background(), CollectRequest{
		RunID: 1, JobName: "build", Workspace: src, Paths: []string{"dist/"},
	})
	if err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	if err := s.Restore(files[0].StorePath, dest, files[0].Path); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dest, "dist", "output.txt"))
	if err != nil {
		t.Fatalf("restored artifact missing: %v", err)
	}
	if string(body) != "artifact body\n" {
		t.Errorf("restored content = %q", body)
	}
}

func TestRestoreRejectsEscapingDestination(t *testing.T) {
	src := t.TempDir()
	writeFile(t, src, "file.txt", "x")
	s := newStore(t)
	files, err := s.Collect(context.Background(), CollectRequest{
		RunID: 1, JobName: "j", Workspace: src, Paths: []string{"file.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	for _, rel := range []string{"../escaped.txt", "/tmp/escaped.txt", "a/../../escaped.txt"} {
		if err := s.Restore(files[0].StorePath, dest, rel); !errors.Is(err, ErrPathEscape) {
			t.Errorf("Restore(dest=%q) error = %v, want ErrPathEscape", rel, err)
		}
	}
	// A hostile store path is rejected too.
	if err := s.Restore("../../../etc/passwd", dest, "ok.txt"); !errors.Is(err, ErrPathEscape) {
		t.Errorf("Restore with an escaping store path: error = %v, want ErrPathEscape", err)
	}
}

func TestRemoveAndPrune(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "dist/output.txt", "x")
	s := newStore(t)
	files, err := s.Collect(context.Background(), CollectRequest{
		RunID: 9, JobName: "build", Workspace: ws, Paths: []string{"dist/"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Remove(files[0].StorePath); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := s.Stat(files[0].StorePath); err == nil {
		t.Error("artifact still present after Remove()")
	}
	// Directories left empty are pruned, but never the store root.
	if _, err := os.Stat(filepath.Join(s.Root(), "9")); !os.IsNotExist(err) {
		t.Error("empty run directory was not pruned")
	}
	if _, err := os.Stat(s.Root()); err != nil {
		t.Errorf("store root was removed: %v", err)
	}
	// Removing again is not an error.
	if err := s.Remove(files[0].StorePath); err != nil {
		t.Errorf("second Remove() error = %v, want nil", err)
	}
}

func TestRemoveRun(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "a.txt", "x")
	s := newStore(t)
	if _, err := s.Collect(context.Background(), CollectRequest{
		RunID: 3, JobName: "j", Workspace: ws, Paths: []string{"a.txt"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRun(3); err != nil {
		t.Fatalf("RemoveRun() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "3")); !os.IsNotExist(err) {
		t.Error("run directory survived RemoveRun()")
	}
}

func TestDownloadName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"dist/output.txt", "output.txt"},
		{"output.txt", "output.txt"},
		{"a/b/c/d.tar.gz", "d.tar.gz"},
		{"", "artifact"},
	}
	for _, tc := range tests {
		if got := DownloadName(tc.in); got != tc.want {
			t.Errorf("DownloadName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
