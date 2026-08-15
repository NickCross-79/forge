// Package cli implements the forge command-line interface.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/nickcross-79/forge/internal/artifacts"
	"github.com/nickcross-79/forge/internal/config"
	"github.com/nickcross-79/forge/internal/observability"
	"github.com/nickcross-79/forge/internal/scheduler"
	"github.com/nickcross-79/forge/internal/secrets"
	"github.com/nickcross-79/forge/internal/store"
	"github.com/spf13/cobra"
)

// globals holds the flags shared by every command.
type globals struct {
	configPath string
	dir        string
	jsonOutput bool
	noColor    bool
	verbose    bool
	quiet      bool
	// logLevel is the application-log level a command wants when neither
	// --verbose nor --quiet was given. `forge run` lowers it, because the
	// followed job output and the summary table already show progress and the
	// scheduler's own INFO lines only repeat them.
	logLevel slog.Level
}

// app is the lazily-initialised runtime shared by commands that need storage.
type app struct {
	cfg       *config.Config
	db        *store.DB
	sched     *scheduler.Scheduler
	secrets   *secrets.Store
	artifacts *artifacts.Store
	logger    *slog.Logger

	out io.Writer
	err io.Writer
	g   *globals
}

// Execute runs the CLI and returns the process exit code.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	root, g := newRootCommand(stdout, stderr)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	if err := root.ExecuteContext(ctx); err != nil {
		// Cobra already prints usage errors; anything else is reported here.
		var silent silentError
		if errors.As(err, &silent) {
			return silent.code
		}
		if g.jsonOutput {
			_ = json.NewEncoder(stderr).Encode(map[string]string{"error": err.Error()})
		} else {
			fail(stderr, "%v", err)
		}
		return 1
	}
	return 0
}

// silentError carries an exit code for a failure that has already been reported
// in full, so the top level does not print a second, less useful message.
type silentError struct {
	code int
}

func (e silentError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func newRootCommand(stdout, stderr io.Writer) (*cobra.Command, *globals) {
	g := &globals{}

	root := &cobra.Command{
		Use:   "forge",
		Short: "A local-first CI/CD runner",
		Long: `forge runs CI/CD pipelines on your own machine.

Define a pipeline in YAML, run it with dependency-aware concurrency, and inspect
logs, artifacts and history from the CLI or the built-in web dashboard. Nothing
leaves your machine and nothing costs anything to run.

Getting started:
  forge init                     scaffold forge.yaml and an example pipeline
  forge validate pipeline.yml    check a pipeline without running it
  forge run pipeline.yml         execute it
  forge serve                    open the dashboard`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version(),
		PersistentPreRun: func(_ *cobra.Command, _ []string) {
			if g.noColor || !shouldColor(stdout) {
				disableColor()
			}
		},
	}

	pf := root.PersistentFlags()
	pf.StringVarP(&g.configPath, "config", "c", "", "path to forge.yaml (default: nearest one found)")
	pf.StringVarP(&g.dir, "dir", "C", "", "project directory (default: current directory)")
	pf.BoolVar(&g.jsonOutput, "json", false, "emit machine-readable JSON")
	pf.BoolVar(&g.noColor, "no-color", false, "disable coloured output")
	pf.BoolVarP(&g.verbose, "verbose", "v", false, "show debug logging")
	pf.BoolVarP(&g.quiet, "quiet", "q", false, "only show warnings and errors")

	root.AddCommand(
		newInitCommand(g, stdout, stderr),
		newValidateCommand(g, stdout, stderr),
		newRunCommand(g, stdout, stderr),
		newPipelinesCommand(g, stdout, stderr),
		newRunsCommand(g, stdout, stderr),
		newRunStatusCommand(g, stdout, stderr),
		newLogsCommand(g, stdout, stderr),
		newArtifactsCommand(g, stdout, stderr),
		newApproveCommand(g, stdout, stderr),
		newCancelCommand(g, stdout, stderr),
		newServeCommand(g, stdout, stderr),
		newPruneCommand(g, stdout, stderr),
	)
	return root, g
}

// version is stamped at build time by the Makefile via -ldflags.
var buildVersion = "dev"

func version() string { return buildVersion }

// loadConfig resolves configuration without touching the database, for commands
// that do not need storage.
func (g *globals) loadConfig() (*config.Config, error) {
	dir := g.dir
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("determine working directory: %w", err)
		}
		dir = wd
	}
	return config.Load(dir, g.configPath)
}

// newApp wires up everything a command needs to touch stored state.
//
// It is called per command rather than at startup so that `forge validate` and
// `forge init` never create a database as a side effect.
func (g *globals) newApp(ctx context.Context, stdout, stderr io.Writer) (*app, error) {
	cfg, err := g.loadConfig()
	if err != nil {
		return nil, err
	}
	if err := cfg.EnsureDirs(); err != nil {
		return nil, err
	}

	level := g.logLevel
	switch {
	case g.verbose:
		level = slog.LevelDebug
	case g.quiet:
		level = slog.LevelWarn
	}
	logger := observability.NewLogger(observability.Options{
		Level:  level,
		Format: observability.FormatText,
		Writer: stderr,
		Color:  !g.noColor && shouldColor(stderr),
	})

	db, err := store.Open(ctx, store.Options{
		Path:        cfg.Database.Path,
		BusyTimeout: cfg.Database.BusyTimeout,
	})
	if err != nil {
		return nil, err
	}

	sec := secrets.New()
	if err := sec.LoadFile(cfg.Secrets.File); err != nil {
		_ = db.Close()
		return nil, err
	}
	sec.LoadEnv(cfg.Secrets.Env)

	art, err := artifacts.NewStore(cfg.Artifacts.Dir)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	art.MaxFileSize = cfg.Artifacts.MaxFileSize
	art.MaxTotalSize = cfg.Artifacts.MaxTotalSize

	sched, err := scheduler.New(scheduler.Options{
		Config:    cfg,
		DB:        db,
		Artifacts: art,
		Secrets:   sec,
		Logger:    logger,
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	return &app{
		cfg: cfg, db: db, sched: sched, secrets: sec, artifacts: art,
		logger: logger, out: stdout, err: stderr, g: g,
	}, nil
}

// Close releases the app's resources.
func (a *app) Close() error {
	if a.db != nil {
		return a.db.Close()
	}
	return nil
}

// writeJSON emits a value as indented JSON, used by every --json code path.
func (a *app) writeJSON(v any) error {
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// writeJSONTo emits JSON without needing a full app, for commands that run
// before storage is initialised.
func writeJSONTo(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
