package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/nickcross-79/forge/internal/logs"
	"github.com/nickcross-79/forge/internal/model"
	"github.com/nickcross-79/forge/internal/store"
	"github.com/spf13/cobra"
)

// newPipelinesCommand lists known pipelines.
func newPipelinesCommand(g *globals, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:     "pipelines",
		Short:   "List pipelines forge has seen",
		Aliases: []string{"pipeline", "pl"},
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.newApp(cmd.Context(), stdout, stderr)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			list, err := a.db.ListPipelines(cmd.Context())
			if err != nil {
				return err
			}
			if g.jsonOutput {
				return a.writeJSON(map[string]any{"pipelines": list})
			}
			if len(list) == 0 {
				hint(stdout, "No pipelines yet. Run one with: forge run pipeline.yml")
				return nil
			}

			t := newTable(stdout)
			t.header("", "NAME", "RUNS", "PASS", "FAIL", "AVG", "LAST", "SOURCE")
			for _, p := range list {
				glyph := " "
				if p.Stats.LastStatus != "" {
					glyph = statusGlyph(p.Stats.LastStatus)
				}
				last := "never"
				if p.Stats.LastRunID > 0 {
					last = fmt.Sprintf("#%d", p.Stats.LastRunID)
				}
				t.row(
					glyph,
					p.Name,
					strconv.FormatInt(p.Stats.Runs, 10),
					strconv.FormatInt(p.Stats.Successes, 10),
					strconv.FormatInt(p.Stats.Failures, 10),
					formatDuration(p.Stats.AvgDuration),
					last,
					truncate(p.SourcePath, 40),
				)
			}
			t.flush()
			return nil
		},
	}
}

// newRunsCommand lists run history.
func newRunsCommand(g *globals, stdout, stderr io.Writer) *cobra.Command {
	var (
		limit      int
		statusFlag []string
		pipeline   string
	)

	cmd := &cobra.Command{
		Use:     "runs",
		Short:   "List recent runs",
		Aliases: []string{"history", "ls"},
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.newApp(cmd.Context(), stdout, stderr)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			filter := store.RunFilter{Limit: limit}
			for _, s := range statusFlag {
				filter.Status = append(filter.Status, model.Status(s))
			}
			if pipeline != "" {
				p, err := a.db.PipelineByName(cmd.Context(), pipeline)
				if err != nil {
					return err
				}
				filter.PipelineID = p.ID
			}

			runs, err := a.db.ListRuns(cmd.Context(), filter)
			if err != nil {
				return err
			}
			if g.jsonOutput {
				return a.writeJSON(map[string]any{"runs": runs})
			}
			if len(runs) == 0 {
				hint(stdout, "No runs match. Start one with: forge run pipeline.yml")
				return nil
			}

			t := newTable(stdout)
			t.header("", "ID", "PIPELINE", "#", "STATUS", "TIME", "STARTED", "TRIGGER")
			for _, run := range runs {
				t.row(
					statusGlyph(run.Status),
					strconv.FormatInt(run.ID, 10),
					run.PipelineName,
					strconv.FormatInt(run.Number, 10),
					colorStatus(run.Status),
					formatDuration(run.DurationMS),
					formatRelative(run.StartedAt),
					string(run.Trigger),
				)
			}
			t.flush()
			hint(stdout, "\nforge run-status <id>   inspect one run")
			return nil
		},
	}

	f := cmd.Flags()
	f.IntVarP(&limit, "limit", "n", 20, "maximum runs to show")
	f.StringSliceVarP(&statusFlag, "status", "s", nil, "filter by status (repeatable)")
	f.StringVarP(&pipeline, "pipeline", "p", "", "filter by pipeline name")
	return cmd
}

// newRunStatusCommand shows one run in detail.
func newRunStatusCommand(g *globals, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:     "run-status <run-id>",
		Short:   "Show the status of one run",
		Aliases: []string{"status", "show"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.newApp(cmd.Context(), stdout, stderr)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			runID, err := parseID(args[0])
			if err != nil {
				return err
			}
			run, err := a.db.RunByID(cmd.Context(), runID)
			if err != nil {
				return err
			}
			jobs, err := a.db.ListJobs(cmd.Context(), runID)
			if err != nil {
				return err
			}
			arts, err := a.db.ListArtifacts(cmd.Context(), runID)
			if err != nil {
				return err
			}
			events, err := a.db.ListEvents(cmd.Context(), runID, 0, 500)
			if err != nil {
				return err
			}

			if g.jsonOutput {
				return a.writeJSON(map[string]any{
					"run": run, "jobs": jobs, "artifacts": arts, "events": events,
					"active": a.sched.IsActive(runID),
				})
			}

			fmt.Fprintf(stdout, "%s %s  %s %s\n",
				bold(run.PipelineName),
				dim(fmt.Sprintf("run #%d", run.Number)),
				statusGlyph(run.Status), colorStatus(run.Status))
			fmt.Fprintf(stdout, "%s\n", dim(fmt.Sprintf(
				"id %d · %s · started %s · took %s",
				run.ID, run.Trigger, formatRelative(run.StartedAt), formatDuration(run.DurationMS))))
			if run.Error != "" {
				fmt.Fprintf(stdout, "%s %s\n", colorRed+"reason:"+colorReset, run.Error)
			}
			fmt.Fprintln(stdout)

			t := newTable(stdout)
			t.header("", "JOB", "STAGE", "STATUS", "TIME", "TRIES", "EXEC", "DETAIL")
			for _, job := range jobs {
				exit := ""
				if job.ExitCode != nil {
					exit = fmt.Sprintf("exit %d", *job.ExitCode)
				}
				detail := job.Error
				if detail == "" {
					detail = exit
				}
				t.row(
					statusGlyph(job.Status),
					job.Name,
					job.Stage,
					colorStatus(job.Status),
					formatDuration(job.DurationMS),
					fmt.Sprintf("%d/%d", job.Attempts, job.MaxAttempts),
					string(job.Executor),
					truncate(detail, 44),
				)
			}
			t.flush()

			if len(arts) > 0 {
				fmt.Fprintf(stdout, "\n%s\n", dim(fmt.Sprintf("%d artifact(s)", len(arts))))
				for _, art := range arts {
					fmt.Fprintf(stdout, "  %s %s/%s %s\n",
						dim("•"), art.JobName, art.Path, dim(formatBytes(art.Size)))
				}
			}

			// Anything awaiting approval is the most actionable thing on screen.
			for _, job := range jobs {
				if job.Status == model.StatusAwaitingManual {
					fmt.Fprintln(stdout)
					warn(stdout, "job %s is waiting for approval", bold(job.Name))
					hint(stdout, "  forge approve %d %s", run.ID, job.Name)
				}
			}

			hint(stdout, "\nforge logs %d        show captured output", run.ID)
			return nil
		},
	}
}

// newLogsCommand prints captured output.
func newLogsCommand(g *globals, stdout, stderr io.Writer) *cobra.Command {
	var (
		jobName    string
		streamName string
		tailLines  int
	)

	cmd := &cobra.Command{
		Use:   "logs <run-id>",
		Short: "Show captured job output",
		Long: `Print the stdout and stderr captured for a run.

The two streams are stored separately, so --stream can show either on its own.
Secrets are masked at capture time, so what is stored is already redacted.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.newApp(cmd.Context(), stdout, stderr)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			runID, err := parseID(args[0])
			if err != nil {
				return err
			}
			if _, err := a.db.RunByID(cmd.Context(), runID); err != nil {
				return err
			}
			jobs, err := a.db.ListJobs(cmd.Context(), runID)
			if err != nil {
				return err
			}

			type entry struct {
				Job     string       `json:"job"`
				Stream  model.Stream `json:"stream"`
				Attempt int64        `json:"attempt_id"`
				Content string       `json:"content"`
			}
			entries := []entry{}

			for _, job := range jobs {
				if jobName != "" && job.Name != jobName {
					continue
				}
				records, err := a.db.ListLogs(cmd.Context(), job.ID)
				if err != nil {
					return err
				}
				for _, rec := range records {
					if streamName != "" && streamName != "both" && string(rec.Stream) != streamName {
						continue
					}
					var body []byte
					if tailLines > 0 {
						body, err = logs.Tail(rec.Path, tailLines)
					} else {
						body, err = logs.ReadAll(rec.Path)
					}
					if err != nil {
						return err
					}
					if len(body) == 0 {
						continue
					}
					entries = append(entries, entry{
						Job: job.Name, Stream: rec.Stream, Attempt: rec.AttemptID,
						Content: string(body),
					})
				}
			}

			if g.jsonOutput {
				return a.writeJSON(map[string]any{"logs": entries})
			}
			if len(entries) == 0 {
				hint(stdout, "No output was captured for run %d.", runID)
				return nil
			}

			for i, e := range entries {
				if i > 0 {
					fmt.Fprintln(stdout)
				}
				label := fmt.Sprintf("── %s · %s ", e.Job, e.Stream)
				fmt.Fprintf(stdout, "%s%s%s\n", colorDim, label+strings.Repeat("─", maxInt(0, 60-len(label))), colorReset)
				body := e.Content
				if e.Stream == model.StreamStderr {
					body = colorRed + body + colorReset
				}
				fmt.Fprint(stdout, body)
				if !strings.HasSuffix(e.Content, "\n") {
					fmt.Fprintln(stdout)
				}
			}
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVarP(&jobName, "job", "j", "", "only show this job's output")
	f.StringVarP(&streamName, "stream", "s", "", "stdout, stderr or both (default: both)")
	f.IntVarP(&tailLines, "tail", "n", 0, "only show the last N lines of each stream")
	return cmd
}

// newArtifactsCommand lists or extracts collected files.
func newArtifactsCommand(g *globals, stdout, stderr io.Writer) *cobra.Command {
	var outputDir string

	cmd := &cobra.Command{
		Use:   "artifacts <run-id>",
		Short: "List or download a run's artifacts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.newApp(cmd.Context(), stdout, stderr)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			runID, err := parseID(args[0])
			if err != nil {
				return err
			}
			if _, err := a.db.RunByID(cmd.Context(), runID); err != nil {
				return err
			}
			list, err := a.db.ListArtifacts(cmd.Context(), runID)
			if err != nil {
				return err
			}

			if outputDir != "" {
				extracted := make([]string, 0, len(list))
				for _, art := range list {
					// Restore validates both ends of the copy, so a hostile stored
					// path cannot write outside the chosen directory.
					if err := a.artifacts.Restore(art.StorePath, outputDir, art.Path); err != nil {
						return fmt.Errorf("extract %s: %w", art.Path, err)
					}
					extracted = append(extracted, art.Path)
				}
				if g.jsonOutput {
					return a.writeJSON(map[string]any{"extracted": extracted, "dir": outputDir})
				}
				success(stdout, "extracted %d artifact(s) to %s", len(extracted), bold(outputDir))
				return nil
			}

			if g.jsonOutput {
				return a.writeJSON(map[string]any{"artifacts": list})
			}
			if len(list) == 0 {
				hint(stdout, "Run %d produced no artifacts.", runID)
				return nil
			}

			var total int64
			t := newTable(stdout)
			t.header("ID", "JOB", "PATH", "SIZE", "SHA256")
			for _, art := range list {
				total += art.Size
				t.row(
					strconv.FormatInt(art.ID, 10),
					art.JobName,
					art.Path,
					formatBytes(art.Size),
					truncate(art.SHA256, 12),
				)
			}
			t.flush()
			fmt.Fprintf(stdout, "\n%s\n", dim(fmt.Sprintf("%d file(s), %s total", len(list), formatBytes(total))))
			hint(stdout, "forge artifacts %d --output ./out   extract them", runID)
			return nil
		},
	}

	cmd.Flags().StringVarP(&outputDir, "output", "o", "", "extract artifacts into this directory")
	return cmd
}

// newApproveCommand releases a manual job.
func newApproveCommand(g *globals, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "approve <run-id> <job>",
		Short: "Approve a manual job that is waiting",
		Long: `Release a job that is blocked on a manual gate.

The run must be executing in a forge process that shares this project — either a
` + "`forge run --wait-manual`" + ` in another terminal, or ` + "`forge serve`" + `.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.newApp(cmd.Context(), stdout, stderr)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			runID, err := parseID(args[0])
			if err != nil {
				return err
			}
			// Approval targets the process actually running the pipeline, so a
			// separate CLI invocation can only reach it through the HTTP API.
			return a.forwardToServer(cmd.Context(), "POST",
				fmt.Sprintf("/api/v1/runs/%d/jobs/%s/approve", runID, args[1]),
				fmt.Sprintf("approved %s in run %d", bold(args[1]), runID), stdout)
		},
	}
}

// newCancelCommand stops an active run.
func newCancelCommand(g *globals, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <run-id>",
		Short: "Cancel a running pipeline",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.newApp(cmd.Context(), stdout, stderr)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			runID, err := parseID(args[0])
			if err != nil {
				return err
			}
			return a.forwardToServer(cmd.Context(), "POST",
				fmt.Sprintf("/api/v1/runs/%d/cancel", runID),
				fmt.Sprintf("cancelled run %d", runID), stdout)
		},
	}
}

// newPruneCommand trims stored history.
func newPruneCommand(g *globals, stdout, stderr io.Writer) *cobra.Command {
	var (
		keep   int
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete old runs, logs and expired artifacts",
		Long: `Remove history beyond the retention settings.

Active runs are never pruned. With --dry-run nothing is deleted and the command
reports what it would remove.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.newApp(cmd.Context(), stdout, stderr)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			if keep <= 0 {
				keep = a.cfg.Workspace.KeepRuns
			}
			report, err := a.prune(cmd.Context(), keep, dryRun)
			if err != nil {
				return err
			}

			if g.jsonOutput {
				return a.writeJSON(report)
			}
			verb := "removed"
			if dryRun {
				verb = "would remove"
			}
			success(stdout, "%s %d run(s), %d expired artifact(s), %s of run workspaces",
				verb, report.Runs, report.Artifacts, formatBytes(report.Bytes))
			return nil
		},
	}

	f := cmd.Flags()
	f.IntVar(&keep, "keep", 0, "runs to keep per pipeline (default: workspace.keep_runs)")
	f.BoolVar(&dryRun, "dry-run", false, "report what would be removed without deleting")
	return cmd
}

func parseID(raw string) (int64, error) {
	// Accept "#12" as well as "12" so a value copied from the UI works.
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "#")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not a valid run id", raw)
	}
	return id, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// readLogChunk is a thin wrapper so the follow loop reads the same way the API does.
func readLogChunk(path string, offset int64) ([]byte, int64, error) {
	return logs.ReadFrom(path, offset, logs.DefaultChunkSize)
}
