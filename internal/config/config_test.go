package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultResolvesPaths(t *testing.T) {
	root := t.TempDir()
	cfg := Default(root)

	// Default must hand back a usable config, not one that still needs
	// normalising — otherwise a caller that never loads a file writes to the
	// wrong place.
	if cfg.Home != filepath.Join(root, HomeDirName) {
		t.Errorf("Home = %q", cfg.Home)
	}
	for name, path := range map[string]string{
		"database":  cfg.Database.Path,
		"artifacts": cfg.Artifacts.Dir,
		"logs":      cfg.Logs.Dir,
		"secrets":   cfg.Secrets.File,
	} {
		if !filepath.IsAbs(path) {
			t.Errorf("%s path %q is not absolute", name, path)
		}
		if !strings.HasPrefix(path, cfg.Home) {
			t.Errorf("%s path %q is not inside the forge home %q", name, path, cfg.Home)
		}
	}
	if cfg.Runner.Concurrency <= 0 {
		t.Error("default concurrency should be positive")
	}
	if !cfg.LoopbackHost() {
		t.Error("the default bind address must be loopback")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the default config does not validate: %v", err)
	}
}

func TestLoadWithoutAConfigFile(t *testing.T) {
	root := t.TempDir()
	cfg, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Path != "" {
		t.Errorf("Path = %q, want empty when defaults were used", cfg.Path)
	}
	if cfg.Root != root {
		t.Errorf("Root = %q, want %q", cfg.Root, root)
	}
}

func TestLoadAppliesFileOverrides(t *testing.T) {
	root := t.TempDir()
	body := `
runner:
  concurrency: 9
  default_executor: docker
  default_timeout: 12m
workspace:
  mode: shared
  keep_runs: 5
server:
  port: 9999
artifacts:
  retention: 48h
`
	if err := os.WriteFile(filepath.Join(root, DefaultFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Runner.Concurrency != 9 {
		t.Errorf("Concurrency = %d, want 9", cfg.Runner.Concurrency)
	}
	if cfg.Runner.DefaultExecutor != "docker" {
		t.Errorf("DefaultExecutor = %q", cfg.Runner.DefaultExecutor)
	}
	if cfg.Runner.DefaultTimeout != 12*time.Minute {
		t.Errorf("DefaultTimeout = %v, want 12m", cfg.Runner.DefaultTimeout)
	}
	if cfg.Workspace.Mode != WorkspaceShared {
		t.Errorf("Workspace.Mode = %q", cfg.Workspace.Mode)
	}
	if cfg.Server.Port != 9999 {
		t.Errorf("Port = %d, want 9999", cfg.Server.Port)
	}
	if cfg.Artifacts.Retention != 48*time.Hour {
		t.Errorf("Retention = %v, want 48h", cfg.Artifacts.Retention)
	}
	if cfg.Path == "" {
		t.Error("Path should record the file that was loaded")
	}
	// Anything the file did not mention keeps its default.
	if cfg.Logs.MaxSize == 0 {
		t.Error("unspecified settings should keep their defaults")
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, DefaultFileName),
		[]byte("runner:\n  concurency: 4\n"), 0o600); err != nil { // typo
		t.Fatal(err)
	}
	_, err := Load(root, "")
	if err == nil {
		t.Fatal("Load() accepted an unknown key")
	}
	if !strings.Contains(err.Error(), "concurency") {
		t.Errorf("error = %v, should name the offending key", err)
	}
}

func TestLoadExplainsBareNumberDurations(t *testing.T) {
	// `manual_timeout: 0` is the easiest mistake to make in this file, and yaml's
	// own message does not say how to fix it.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, DefaultFileName),
		[]byte("runner:\n  manual_timeout: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(root, "")
	if err == nil {
		t.Fatal("Load() accepted a bare number for a duration")
	}
	if !strings.Contains(err.Error(), "0s") {
		t.Errorf("error = %v, should suggest writing the duration with a unit", err)
	}
}

func TestHomeOverrideRelocatesDerivedPaths(t *testing.T) {
	root := t.TempDir()
	custom := filepath.Join(root, "custom-home")
	if err := os.WriteFile(filepath.Join(root, DefaultFileName),
		[]byte("home: custom-home\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(root, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Home != custom {
		t.Fatalf("Home = %q, want %q", cfg.Home, custom)
	}
	// The database, artifacts and logs must follow the home, not stay behind in
	// the default location.
	for name, path := range map[string]string{
		"database":  cfg.Database.Path,
		"artifacts": cfg.Artifacts.Dir,
		"logs":      cfg.Logs.Dir,
		"secrets":   cfg.Secrets.File,
	} {
		if !strings.HasPrefix(path, custom) {
			t.Errorf("%s = %q, want it under the overridden home %q", name, path, custom)
		}
	}
}

func TestExplicitRelativePathsResolveAgainstHome(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, DefaultFileName),
		[]byte("artifacts:\n  dir: my-artifacts\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, HomeDirName, "my-artifacts")
	if cfg.Artifacts.Dir != want {
		t.Errorf("Artifacts.Dir = %q, want %q", cfg.Artifacts.Dir, want)
	}
}

func TestAbsolutePathsAreKept(t *testing.T) {
	root := t.TempDir()
	absolute := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(filepath.Join(root, DefaultFileName),
		[]byte("artifacts:\n  dir: "+absolute+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Artifacts.Dir != absolute {
		t.Errorf("Artifacts.Dir = %q, want the absolute path preserved", cfg.Artifacts.Dir)
	}
}

func TestEnvOverrides(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FORGE_CONCURRENCY", "13")
	t.Setenv("FORGE_PORT", "8123")
	t.Setenv("FORGE_TOKEN", "from-env")

	cfg, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runner.Concurrency != 13 {
		t.Errorf("Concurrency = %d, want 13 from the environment", cfg.Runner.Concurrency)
	}
	if cfg.Server.Port != 8123 {
		t.Errorf("Port = %d, want 8123", cfg.Server.Port)
	}
	if cfg.Server.Token != "from-env" {
		t.Errorf("Token = %q", cfg.Server.Token)
	}
}

func TestEnvOverridesBeatTheFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, DefaultFileName),
		[]byte("runner:\n  concurrency: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORGE_CONCURRENCY", "7")

	cfg, err := Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runner.Concurrency != 7 {
		t.Errorf("Concurrency = %d, want the environment to win", cfg.Runner.Concurrency)
	}
}

func TestValidateRefusesUnsafeBinds(t *testing.T) {
	// The API can start pipelines, so exposing it without a token would hand
	// anyone on the network a shell.
	cfg := Default(t.TempDir())
	cfg.Server.Host = "0.0.0.0"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a non-loopback bind with no token was accepted")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error = %v, should explain that a token is required", err)
	}

	cfg.Server.Token = "example-not-a-real-token"
	if err := cfg.Validate(); err != nil {
		t.Errorf("a non-loopback bind with a token should be allowed: %v", err)
	}
}

func TestValidateCollectsEveryProblem(t *testing.T) {
	cfg := Default(t.TempDir())
	cfg.Runner.Concurrency = -1
	cfg.Runner.DefaultExecutor = "podman"
	cfg.Workspace.Mode = "nonsense"
	cfg.Server.Port = 99999

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() returned nil for an invalid config")
	}
	for _, want := range []string{"concurrency", "default_executor", "workspace.mode", "server.port"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q:\n%v", want, err)
		}
	}
}

func TestLoopbackDetection(t *testing.T) {
	loopback := []string{"127.0.0.1", "localhost", "::1", "[::1]", "LOCALHOST", " 127.0.0.1 "}
	for _, host := range loopback {
		cfg := Default(t.TempDir())
		cfg.Server.Host = host
		if !cfg.LoopbackHost() {
			t.Errorf("LoopbackHost(%q) = false, want true", host)
		}
	}
	for _, host := range []string{"0.0.0.0", "192.168.1.5", "example.com", "::"} {
		cfg := Default(t.TempDir())
		cfg.Server.Host = host
		if cfg.LoopbackHost() {
			t.Errorf("LoopbackHost(%q) = true, want false", host)
		}
	}
}

func TestDiscoverWalksUp(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, DefaultFileName), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Running forge from a subdirectory should find the project root, the way
	// git does.
	if got := Discover(nested); got != root {
		t.Errorf("Discover(%q) = %q, want %q", nested, got, root)
	}

	// A .forge directory marks the root too.
	other := t.TempDir()
	deep := filepath.Join(other, "x", "y")
	if err := os.MkdirAll(deep, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(other, HomeDirName), 0o750); err != nil {
		t.Fatal(err)
	}
	if got := Discover(deep); got != other {
		t.Errorf("Discover(%q) = %q, want %q", deep, got, other)
	}
}

func TestDiscoverFallsBackToTheGivenDirectory(t *testing.T) {
	dir := t.TempDir()
	if got := Discover(dir); got != dir {
		t.Errorf("Discover() = %q, want the directory itself when nothing is found", got)
	}
}

func TestEnsureDirsAndAccessors(t *testing.T) {
	root := t.TempDir()
	cfg := Default(root)
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs() error = %v", err)
	}
	for _, dir := range []string{cfg.Home, cfg.Artifacts.Dir, cfg.Logs.Dir, cfg.RunsDir()} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Errorf("%s was not created: %v", dir, err)
		}
	}

	if got := cfg.RunDir(42); got != filepath.Join(cfg.RunsDir(), "42") {
		t.Errorf("RunDir(42) = %q", got)
	}
	if got := cfg.Addr(); got != "127.0.0.1:7777" {
		t.Errorf("Addr() = %q", got)
	}
}

func TestMarshalRoundTrips(t *testing.T) {
	cfg := Default(t.TempDir())
	body, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(body), "concurrency") {
		t.Errorf("marshalled config looks wrong:\n%s", body)
	}
}

func TestLoadExplicitPathMustExist(t *testing.T) {
	if _, err := Load(t.TempDir(), filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Error("Load() with an explicit missing path should fail")
	}
}
