// Package config loads and validates forge's configuration and resolves the
// on-disk layout of the forge home directory.
//
// Every setting has a working default, so forge runs with no config file at all.
// A `forge.yaml` at the project root (or the path given by --config) overrides
// defaults, and a handful of FORGE_* environment variables override that.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultFileName is the config file forge looks for at the project root.
const DefaultFileName = "forge.yaml"

// HomeDirName is the per-project directory holding the database, logs and artifacts.
const HomeDirName = ".forge"

// WorkspaceMode selects how job working directories are provisioned.
type WorkspaceMode string

const (
	// WorkspaceIsolated gives every job its own copy of the project tree, seeded
	// with the artifacts of the jobs it needs. This is the only mode that is
	// correct when independent jobs run concurrently, so it is the default.
	WorkspaceIsolated WorkspaceMode = "isolated"
	// WorkspaceShared gives every job in a run the same directory. Faster on large
	// trees, but concurrent jobs can interfere with each other.
	WorkspaceShared WorkspaceMode = "shared"
)

// Config is the fully-resolved configuration for a forge project.
type Config struct {
	// Root is the project directory. Pipelines are resolved relative to it and
	// job workspaces are seeded from it.
	Root string `yaml:"-"`
	// Home is the forge state directory, normally <Root>/.forge.
	Home string `yaml:"home,omitempty"`

	Database  DatabaseConfig  `yaml:"database"`
	Runner    RunnerConfig    `yaml:"runner"`
	Workspace WorkspaceConfig `yaml:"workspace"`
	Artifacts ArtifactsConfig `yaml:"artifacts"`
	Logs      LogsConfig      `yaml:"logs"`
	Server    ServerConfig    `yaml:"server"`
	Secrets   SecretsConfig   `yaml:"secrets"`

	// Path is the config file this was loaded from, empty when defaults were used.
	Path string `yaml:"-"`
}

// DatabaseConfig controls the SQLite metadata store.
type DatabaseConfig struct {
	// Path to the SQLite file. Relative paths resolve against Home.
	Path string `yaml:"path"`
	// BusyTimeout bounds how long a writer waits on a locked database.
	BusyTimeout time.Duration `yaml:"busy_timeout"`
}

// RunnerConfig controls how jobs are scheduled and executed.
type RunnerConfig struct {
	// Concurrency caps how many jobs run at once. 0 means NumCPU.
	Concurrency int `yaml:"concurrency"`
	// DefaultExecutor is used by jobs that do not specify an image.
	DefaultExecutor string `yaml:"default_executor"`
	// DefaultTimeout bounds any job that does not set its own timeout.
	DefaultTimeout time.Duration `yaml:"default_timeout"`
	// DefaultRetries is the retry budget for jobs that do not set one.
	DefaultRetries int `yaml:"default_retries"`
	// RetryBackoff is the delay before the first retry; it doubles each attempt.
	RetryBackoff time.Duration `yaml:"retry_backoff"`
	// Shell overrides the interpreter used for local execution.
	Shell string `yaml:"shell"`
	// DockerImage is the fallback image for docker jobs that omit one.
	DockerImage string `yaml:"docker_image"`
	// EnvPassthrough lists host environment variables jobs may inherit. Anything
	// not listed is withheld, so the host environment is not leaked wholesale.
	EnvPassthrough []string `yaml:"env_passthrough"`
	// ManualTimeout bounds how long a manual job waits for approval before being
	// skipped. Zero means wait indefinitely.
	ManualTimeout time.Duration `yaml:"manual_timeout"`
}

// WorkspaceConfig controls how job working directories are provisioned.
type WorkspaceConfig struct {
	Mode WorkspaceMode `yaml:"mode"`
	// Ignore lists path patterns excluded when seeding a workspace from the
	// project tree. Matched against each entry's path relative to the root.
	Ignore []string `yaml:"ignore"`
	// KeepRuns limits how many run directories are retained on disk. 0 keeps all.
	KeepRuns int `yaml:"keep_runs"`
}

// ArtifactsConfig controls artifact collection and retention.
type ArtifactsConfig struct {
	// Dir holds collected artifacts. Relative paths resolve against Home.
	Dir string `yaml:"dir"`
	// Retention is how long artifacts are kept. Zero means keep forever.
	Retention time.Duration `yaml:"retention"`
	// MaxFileSize rejects individual files larger than this. Zero means no limit.
	MaxFileSize int64 `yaml:"max_file_size"`
	// MaxTotalSize caps the total bytes collected per job. Zero means no limit.
	MaxTotalSize int64 `yaml:"max_total_size"`
}

// LogsConfig controls log capture.
type LogsConfig struct {
	// Dir holds captured logs. Relative paths resolve against Home.
	Dir string `yaml:"dir"`
	// MaxSize truncates a single stream of a single attempt beyond this many
	// bytes, so a runaway job cannot fill the disk. Zero means no limit.
	MaxSize int64 `yaml:"max_size"`
	// Retention is how long logs are kept. Zero means keep forever.
	Retention time.Duration `yaml:"retention"`
}

// ServerConfig controls the HTTP API and dashboard.
type ServerConfig struct {
	// Host to bind. Defaults to loopback: the API can start arbitrary shell
	// commands, so it must not be exposed by accident.
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// Token, when set, is required as `Authorization: Bearer <token>`. Binding to
	// a non-loopback address without a token is refused.
	Token string `yaml:"token"`
	// CORSOrigins allows the Vite dev server to talk to the API during development.
	CORSOrigins []string `yaml:"cors_origins"`
}

// SecretsConfig controls where secrets are read from.
type SecretsConfig struct {
	// File is a KEY=VALUE file, by default <Home>/secrets.env. Values are injected
	// into job environments and redacted from logs, and are never written to SQLite.
	File string `yaml:"file"`
	// Env lists host environment variables to treat as secrets: they are passed to
	// jobs and their values are masked in captured output.
	Env []string `yaml:"env"`
}

// Default returns the configuration used when no file is present. root should be
// the project directory.
//
// Paths in the returned config are already resolved against the forge home, so a
// caller that never loads a file still gets a usable config. Load re-resolves
// after decoding, which is what lets a config file override individual paths.
func Default(root string) *Config {
	return defaultWithHome(root, filepath.Join(root, HomeDirName))
}

// defaultWithHome builds the defaults rooted at a specific forge home. Load uses
// it so that a `home:` key in the config file relocates the database, artifacts
// and logs along with it, rather than leaving them behind in the default home.
func defaultWithHome(root, home string) *Config {
	if home == "" {
		home = filepath.Join(root, HomeDirName)
	}
	if !filepath.IsAbs(home) {
		home = filepath.Join(root, home)
	}
	return &Config{
		Root: root,
		Home: home,
		Database: DatabaseConfig{
			Path:        filepath.Join(home, "forge.db"),
			BusyTimeout: 5 * time.Second,
		},
		Runner: RunnerConfig{
			Concurrency:     runtime.NumCPU(),
			DefaultExecutor: "local",
			DefaultTimeout:  30 * time.Minute,
			DefaultRetries:  0,
			RetryBackoff:    2 * time.Second,
			DockerImage:     "alpine:3.20",
			EnvPassthrough:  []string{"PATH", "HOME", "LANG", "LC_ALL", "TZ", "TERM", "USER", "SHELL", "TMPDIR"},
			ManualTimeout:   0,
		},
		Workspace: WorkspaceConfig{
			Mode:     WorkspaceIsolated,
			Ignore:   []string{".git", ".forge", "node_modules", ".venv", "__pycache__"},
			KeepRuns: 50,
		},
		Artifacts: ArtifactsConfig{
			Dir:          filepath.Join(home, "artifacts"),
			Retention:    30 * 24 * time.Hour,
			MaxFileSize:  256 << 20, // 256 MiB
			MaxTotalSize: 1 << 30,   // 1 GiB
		},
		Logs: LogsConfig{
			Dir:       filepath.Join(home, "logs"),
			MaxSize:   16 << 20, // 16 MiB per stream per attempt
			Retention: 30 * 24 * time.Hour,
		},
		Server: ServerConfig{
			Host:        "127.0.0.1",
			Port:        7777,
			CORSOrigins: []string{"http://localhost:5173", "http://127.0.0.1:5173"},
		},
		Secrets: SecretsConfig{
			File: filepath.Join(home, "secrets.env"),
		},
	}
}

// Discover walks up from dir looking for a forge.yaml or a .forge directory and
// returns the project root. It falls back to dir when neither is found, so forge
// works in a directory that has never been initialised.
func Discover(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	for {
		if fileExists(filepath.Join(abs, DefaultFileName)) || dirExists(filepath.Join(abs, HomeDirName)) {
			return abs
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return dir
		}
		abs = parent
	}
}

// Load reads configuration for the project rooted at dir. When explicitPath is
// non-empty that file is required to exist; otherwise a forge.yaml at the project
// root is used when present and defaults are used when it is not.
func Load(dir, explicitPath string) (*Config, error) {
	root := Discover(dir)
	if explicitPath != "" {
		abs, err := filepath.Abs(explicitPath)
		if err != nil {
			return nil, fmt.Errorf("resolve config path: %w", err)
		}
		root = filepath.Dir(abs)
	}
	path := explicitPath
	if path == "" {
		candidate := filepath.Join(root, DefaultFileName)
		if fileExists(candidate) {
			path = candidate
		}
	}

	var raw []byte
	if path != "" {
		data, err := os.ReadFile(path) //nolint:gosec // path is operator-supplied by design
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		raw = data
	}

	// The forge home decides where the database, artifacts and logs live, so it
	// has to be known before the defaults for those paths are computed.
	cfg := defaultWithHome(root, peekHome(raw))

	if path != "" {
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, explainDecodeError(err))
		}
		cfg.Path = path
		cfg.Root = root
	}

	applyEnvOverrides(cfg)
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEnvOverrides lets FORGE_* variables win over the config file. This keeps
// container and CI usage simple without a flag for every setting.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("FORGE_HOME"); v != "" {
		cfg.Home = v
	}
	if v := os.Getenv("FORGE_DB"); v != "" {
		cfg.Database.Path = v
	}
	if v := os.Getenv("FORGE_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Runner.Concurrency = n
		}
	}
	if v := os.Getenv("FORGE_HOST"); v != "" {
		cfg.Server.Host = v
	}
	if v := os.Getenv("FORGE_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Server.Port = n
		}
	}
	if v := os.Getenv("FORGE_TOKEN"); v != "" {
		cfg.Server.Token = v
	}
}

// normalize resolves relative paths against Home and fills in zero values that
// mean "pick a sensible default".
func (c *Config) normalize() {
	if c.Home == "" {
		c.Home = filepath.Join(c.Root, HomeDirName)
	}
	if !filepath.IsAbs(c.Home) {
		c.Home = filepath.Join(c.Root, c.Home)
	}
	c.Database.Path = c.resolveHome(c.Database.Path, "forge.db")
	c.Artifacts.Dir = c.resolveHome(c.Artifacts.Dir, "artifacts")
	c.Logs.Dir = c.resolveHome(c.Logs.Dir, "logs")
	c.Secrets.File = c.resolveHome(c.Secrets.File, "secrets.env")

	if c.Runner.Concurrency <= 0 {
		c.Runner.Concurrency = runtime.NumCPU()
	}
	if c.Runner.DefaultExecutor == "" {
		c.Runner.DefaultExecutor = "local"
	}
	if c.Runner.DefaultTimeout <= 0 {
		c.Runner.DefaultTimeout = 30 * time.Minute
	}
	if c.Runner.RetryBackoff <= 0 {
		c.Runner.RetryBackoff = 2 * time.Second
	}
	if c.Workspace.Mode == "" {
		c.Workspace.Mode = WorkspaceIsolated
	}
	if c.Database.BusyTimeout <= 0 {
		c.Database.BusyTimeout = 5 * time.Second
	}
	if c.Server.Host == "" {
		c.Server.Host = "127.0.0.1"
	}
	if c.Server.Port == 0 {
		c.Server.Port = 7777
	}
}

func (c *Config) resolveHome(p, fallback string) string {
	if p == "" {
		p = fallback
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Home, p)
}

// Validate reports configuration that would produce surprising or unsafe behaviour.
func (c *Config) Validate() error {
	var errs []error
	if c.Runner.Concurrency <= 0 {
		errs = append(errs, errors.New("runner.concurrency must be greater than zero"))
	}
	switch c.Runner.DefaultExecutor {
	case "local", "docker":
	default:
		errs = append(errs, fmt.Errorf("runner.default_executor %q must be \"local\" or \"docker\"", c.Runner.DefaultExecutor))
	}
	switch c.Workspace.Mode {
	case WorkspaceIsolated, WorkspaceShared:
	default:
		errs = append(errs, fmt.Errorf("workspace.mode %q must be \"isolated\" or \"shared\"", c.Workspace.Mode))
	}
	if c.Server.Port < 0 || c.Server.Port > 65535 {
		errs = append(errs, fmt.Errorf("server.port %d is out of range", c.Server.Port))
	}
	// Starting a run runs shell commands. Exposing that beyond loopback without a
	// shared secret would hand anyone on the network a remote shell.
	if !isLoopback(c.Server.Host) && c.Server.Token == "" {
		errs = append(errs, fmt.Errorf(
			"server.host %q is not loopback: set server.token (or FORGE_TOKEN) before exposing the API, which can execute shell commands",
			c.Server.Host))
	}
	return errors.Join(errs...)
}

// isLoopback reports whether binding to host keeps the API on this machine.
func isLoopback(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	default:
		return false
	}
}

// LoopbackHost reports whether the configured bind address is loopback-only.
func (c *Config) LoopbackHost() bool { return isLoopback(c.Server.Host) }

// Addr is the host:port the HTTP server binds.
func (c *Config) Addr() string { return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port) }

// RunsDir is the parent directory for per-run workspaces.
func (c *Config) RunsDir() string { return filepath.Join(c.Home, "runs") }

// RunDir is the directory holding one run's workspaces.
func (c *Config) RunDir(runID int64) string {
	return filepath.Join(c.RunsDir(), strconv.FormatInt(runID, 10))
}

// EnsureDirs creates the directory layout forge writes into.
func (c *Config) EnsureDirs() error {
	for _, dir := range []string{
		c.Home,
		filepath.Dir(c.Database.Path),
		c.Artifacts.Dir,
		c.Logs.Dir,
		c.RunsDir(),
	} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

// Marshal renders the config as YAML, used by `forge init` and GET /settings.
func (c *Config) Marshal() ([]byte, error) { return yaml.Marshal(c) }

// peekHome reads just the `home` key, ignoring everything else and any error:
// a malformed document is reported properly by the full decode that follows.
func peekHome(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var probe struct {
		Home string `yaml:"home"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.Home
}

// explainDecodeError adds a hint to the error yaml produces for a bare number
// where a duration was expected, which is by far the easiest mistake to make in
// this file.
func explainDecodeError(err error) error {
	if err == nil || !strings.Contains(err.Error(), "into time.Duration") {
		return err
	}
	return fmt.Errorf("%w (durations need a unit, for example 30m, 1h30m, 500ms "+
		"or 0s; a bare 0 is a number, not a duration)", err)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
