package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nickcross-79/forge/internal/model"
	"github.com/nickcross-79/forge/internal/pipeline"
	"github.com/nickcross-79/forge/internal/scheduler"
	"github.com/spf13/cobra"
)

// newRunCommand executes a pipeline in the foreground.
func newRunCommand(g *globals, stdout, stderr io.Writer) *cobra.Command {
	var (
		concurrency int
		approve     []string
		waitManual  bool
		variables   []string
		follow      bool
	)

	cmd := &cobra.Command{
		Use:   "run [pipeline.yml]",
		Short: "Execute a pipeline",
		Long: `Run a pipeline to completion, streaming progress as jobs start and finish.

Jobs with no unmet dependencies run concurrently up to the configured limit.
The exit status is 0 when the run succeeds and 1 when it fails or is cancelled,
so this is safe to use as a git hook or in a script.

Manual jobs are skipped by default, because a terminal run has nobody to approve
them. Use --approve to release specific jobs, or --wait-manual to block until
they are approved from another terminal or the dashboard.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Job output and the summary carry the progress story here, so the
			// scheduler's own logs stay out of the way unless asked for.
			g.logLevel = slog.LevelWarn

			a, err := g.newApp(cmd.Context(), stdout, stderr)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			var arg string
			if len(args) > 0 {
				arg = args[0]
			}
			path, err := resolvePipelinePath(arg, a.cfg.Root)
			if err != nil {
				return err
			}

			spec, graph, err := pipeline.Load(path)
			if err != nil {
				return err
			}

			overrides, err := parseKeyValues(variables)
			if err != nil {
				return err
			}
			manual := scheduler.ManualSkip
			if waitManual {
				manual = scheduler.ManualWait
			}

			opts := scheduler.RunOptions{
				Trigger:     model.TriggerCLI,
				Concurrency: concurrency,
				Manual:      manual,
				Approved:    approve,
				Variables:   overrides,
				SourcePath:  path,
			}

			run, err := a.sched.Prepare(cmd.Context(), spec, graph, opts)
			if err != nil {
				return err
			}

			if !g.jsonOutput && !g.quiet {
				fmt.Fprintf(stdout, "%s %s %s\n",
					bold(spec.Name), dim("run"), bold(fmt.Sprintf("#%d", run.Number)))
				fmt.Fprintf(stdout, "%s\n\n", dim(fmt.Sprintf(
					"%d job(s), concurrency %d, %s workspaces",
					len(graph.Nodes), effectiveConcurrency(concurrency, a.cfg.Runner.Concurrency),
					a.cfg.Workspace.Mode)))
			}

			// Ctrl-C cancels the run rather than killing forge outright, so job
			// processes are stopped cleanly and the outcome is still recorded.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// The watcher must not fire on the deferred stop() at the end of a
			// clean run, so it races the run's own completion.
			runComplete := make(chan struct{})
			defer close(runComplete)
			go func() {
				select {
				case <-ctx.Done():
					if !g.quiet && !g.jsonOutput {
						fmt.Fprintf(stderr, "\n%s\n", dim("interrupt received, cancelling the run…"))
					}
				case <-runComplete:
				}
			}()

			var doneFollowing func()
			if follow && !g.jsonOutput {
				doneFollowing = a.followRun(ctx, run.ID, stdout)
			}

			final, err := a.sched.Execute(ctx, run, spec, graph, opts)
			if doneFollowing != nil {
				doneFollowing()
			}
			if err != nil {
				return err
			}

			jobs, err := a.db.ListJobs(context.WithoutCancel(ctx), final.ID)
			if err != nil {
				return err
			}

			if g.jsonOutput {
				if err := a.writeJSON(map[string]any{"run": final, "jobs": jobs}); err != nil {
					return err
				}
			} else {
				printRunSummary(stdout, final, jobs)
			}

			if final.Status != model.StatusSuccess {
				return silentError{code: 1}
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.IntVarP(&concurrency, "concurrency", "j", 0, "maximum jobs to run at once (default: from config)")
	f.StringSliceVar(&approve, "approve", nil, "pre-approve these manual jobs")
	f.BoolVar(&waitManual, "wait-manual", false, "block on manual jobs instead of skipping them")
	f.StringSliceVarP(&variables, "var", "e", nil, "override a pipeline variable (KEY=VALUE, repeatable)")
	f.BoolVar(&follow, "follow", true, "stream job output as it happens")
	return cmd
}

// followRun tails job output while the run executes and returns a function that
// stops the tailing.
//
// Each job's stdout is prefixed with its name, which is what makes concurrent
// output readable: without the prefix, interleaved lines from parallel jobs are
// impossible to attribute.
func (a *app) followRun(ctx context.Context, runID int64, w io.Writer) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		offsets := make(map[int64]int64)
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()

		drain := func() {
			jobs, err := a.db.ListJobs(context.WithoutCancel(ctx), runID)
			if err != nil {
				return
			}
			for _, job := range jobs {
				records, err := a.db.ListLogs(context.WithoutCancel(ctx), job.ID)
				if err != nil {
					continue
				}
				for _, rec := range records {
					if rec.Stream != model.StreamStdout {
						continue
					}
					data, next, err := readLogChunk(rec.Path, offsets[rec.ID])
					if err != nil || len(data) == 0 {
						continue
					}
					offsets[rec.ID] = next
					prefix := dim(fmt.Sprintf("%-14s│ ", truncate(job.Name, 13)))
					for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
						fmt.Fprintf(w, "%s%s\n", prefix, line)
					}
				}
			}
		}

		for {
			select {
			case <-ctx.Done():
				drain() // one last pass so nothing written at the end is lost
				return
			case <-ticker.C:
				drain()
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

// printRunSummary renders the per-job outcome table and the headline result.
func printRunSummary(w io.Writer, run *model.Run, jobs []*model.Job) {
	fmt.Fprintln(w)
	t := newTable(w)
	t.header("", "JOB", "STAGE", "STATUS", "TIME", "DETAIL")
	for _, job := range jobs {
		detail := job.Error
		if job.Status == model.StatusSuccess && job.Attempts > 1 {
			detail = fmt.Sprintf("succeeded after %d attempts", job.Attempts)
		}
		t.row(
			statusGlyph(job.Status),
			job.Name,
			job.Stage,
			colorStatus(job.Status),
			formatDuration(job.DurationMS),
			truncate(detail, 52),
		)
	}
	t.flush()

	fmt.Fprintln(w)
	line := fmt.Sprintf("run #%d %s in %s", run.Number, colorStatus(run.Status), formatDuration(run.DurationMS))
	switch run.Status {
	case model.StatusSuccess:
		success(w, "%s", line)
	case model.StatusFailed:
		fail(w, "%s", line)
		if run.Error != "" {
			fmt.Fprintf(w, "  %s\n", run.Error)
		}
	default:
		warn(w, "%s", line)
	}
	hint(w, "\nforge logs %d        show captured output", run.ID)
	hint(w, "forge artifacts %d   list collected files", run.ID)
}

// effectiveConcurrency resolves the value shown in the run header.
func effectiveConcurrency(override, configured int) int {
	if override > 0 {
		return override
	}
	return configured
}

// parseKeyValues turns repeated KEY=VALUE flags into a map.
func parseKeyValues(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, found := strings.Cut(pair, "=")
		if !found || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("--var %q must be in KEY=VALUE form", pair)
		}
		out[strings.TrimSpace(key)] = value
	}
	return out, nil
}
