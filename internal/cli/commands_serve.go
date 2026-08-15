package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/nickcross-79/forge/internal/api"
	"github.com/spf13/cobra"
)

// newServeCommand starts the API and dashboard.
func newServeCommand(g *globals, stdout, stderr io.Writer) *cobra.Command {
	var (
		host string
		port int
		open bool
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the local API and web dashboard",
		Long: `Serve the REST API and the dashboard from a single process.

The server binds to loopback by default. It can start pipelines, which means it
can run shell commands, so binding to any other address requires server.token to
be set (or FORGE_TOKEN in the environment).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.newApp(cmd.Context(), stdout, stderr)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			if host != "" {
				a.cfg.Server.Host = host
			}
			if port != 0 {
				a.cfg.Server.Port = port
			}
			// Re-validate: the flags above can turn a safe config into an unsafe one.
			if err := a.cfg.Validate(); err != nil {
				return err
			}

			// Runs left mid-flight by a previous process cannot be resumed, so they
			// are reconciled to a truthful terminal state before serving.
			if n, err := a.db.ReconcileInterruptedRuns(cmd.Context()); err != nil {
				a.logger.Warn("failed to reconcile interrupted runs", "error", err)
			} else if n > 0 {
				warn(stderr, "marked %d interrupted run(s) as cancelled", n)
			}

			server, err := api.New(api.Options{
				Config: a.cfg, DB: a.db, Scheduler: a.sched,
				Secrets: a.secrets, Logger: a.logger,
			})
			if err != nil {
				return err
			}

			url := fmt.Sprintf("http://%s", a.cfg.Addr())
			if !g.quiet {
				fmt.Fprintf(stdout, "%s %s\n", bold("forge"), dim("serving on "+url))
				fmt.Fprintf(stdout, "  %s %s\n", dim("dashboard"), url)
				fmt.Fprintf(stdout, "  %s %s\n", dim("api      "), url+"/api/v1")
				fmt.Fprintf(stdout, "  %s %s\n", dim("metrics  "), url+"/metrics")
				if a.cfg.Server.Token != "" {
					fmt.Fprintf(stdout, "  %s %s\n", dim("auth     "), "bearer token required")
				}
				if !a.cfg.LoopbackHost() {
					warn(stdout, "listening beyond loopback: anyone who can reach this port and present the token can run shell commands")
				}
				fmt.Fprintf(stdout, "\n%s\n", dim("Ctrl-C to stop"))
			}

			if open {
				openBrowser(url, a.logger)
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			if err := server.ListenAndServe(ctx); err != nil {
				return err
			}
			if !g.quiet {
				fmt.Fprintf(stdout, "%s\n", dim("stopped"))
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&host, "host", "", "address to bind (default: from config, 127.0.0.1)")
	f.IntVarP(&port, "port", "p", 0, "port to bind (default: from config, 7777)")
	f.BoolVar(&open, "open", false, "open the dashboard in a browser")
	return cmd
}

// openBrowser makes a best-effort attempt to open a URL. Failure is not an
// error: the URL has already been printed.
func openBrowser(url string, logger *slog.Logger) {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name, args = "open", []string{url}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		name, args = "xdg-open", []string{url}
	}
	if err := exec.Command(name, args...).Start(); err != nil { //nolint:gosec // fixed command names
		logger.Debug("could not open a browser", "error", err)
	}
}

// --- talking to a running server -------------------------------------------

// forwardToServer sends a control request to the forge process executing runs.
//
// Cancellation and approval act on in-memory run state, so they only mean
// anything inside the process actually running the pipeline. A separate CLI
// invocation therefore reaches it over the local API rather than editing the
// database directly, which would record a lie and change nothing.
func (a *app) forwardToServer(ctx context.Context, method, path, successMsg string, stdout io.Writer) error {
	url := fmt.Sprintf("http://%s%s", a.cfg.Addr(), path)

	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return err
	}
	if a.cfg.Server.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.Server.Token)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// Any transport failure here means the same thing in practice: there is
		// no forge server on the other end.
		return fmt.Errorf(
			"could not reach a forge server at %s.\n\n"+
				"Cancelling and approving act on a run that is currently executing, so a\n"+
				"forge process has to be running it. Either start the server:\n"+
				"    forge serve\n"+
				"or, for manual approval, run the pipeline with:\n"+
				"    forge run --wait-manual",
			a.cfg.Addr())
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		var parsed struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
			return fmt.Errorf("%s", parsed.Error.Message)
		}
		return fmt.Errorf("server returned %s", resp.Status)
	}

	if a.g.jsonOutput {
		_, err := stdout.Write(body)
		return err
	}
	success(stdout, "%s", successMsg)
	return nil
}

// --- pruning ----------------------------------------------------------------

// pruneReport summarises what a prune removed.
type pruneReport struct {
	Runs      int   `json:"runs"`
	Artifacts int   `json:"artifacts"`
	Bytes     int64 `json:"bytes_freed"`
	DryRun    bool  `json:"dry_run"`
}

// prune trims history: expired artifacts, old runs, and the workspaces that
// belong to them.
func (a *app) prune(ctx context.Context, keep int, dryRun bool) (*pruneReport, error) {
	report := &pruneReport{DryRun: dryRun}

	expired, err := a.db.ExpiredArtifacts(ctx, time.Now())
	if err != nil {
		return nil, err
	}
	for _, art := range expired {
		report.Artifacts++
		report.Bytes += art.Size
		if dryRun {
			continue
		}
		if err := a.artifacts.Remove(art.StorePath); err != nil {
			a.logger.Warn("failed to remove artifact bytes", "artifact", art.ID, "error", err)
		}
		if err := a.db.DeleteArtifact(ctx, art.ID); err != nil {
			return nil, err
		}
	}

	if keep > 0 {
		ids, err := a.db.PrunableRuns(ctx, keep)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			report.Runs++
			report.Bytes += dirSize(a.cfg.RunDir(id))
		}
		if !dryRun {
			if _, err := a.db.PruneRuns(ctx, keep); err != nil {
				return nil, err
			}
			for _, id := range ids {
				if err := os.RemoveAll(a.cfg.RunDir(id)); err != nil {
					a.logger.Warn("failed to remove run workspace", "run", id, "error", err)
				}
				if err := a.artifacts.RemoveRun(id); err != nil {
					a.logger.Warn("failed to remove run artifacts", "run", id, "error", err)
				}
			}
		}
	}
	return report, nil
}

// dirSize totals the bytes under a directory. Files that vanish mid-walk are
// skipped: a partial total is more useful here than an error.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil //nolint:nilerr // see above
		}
		total += info.Size()
		return nil
	})
	return total
}
