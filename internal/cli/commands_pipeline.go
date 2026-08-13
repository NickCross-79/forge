package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickcross-79/forge/internal/config"
	"github.com/nickcross-79/forge/internal/pipeline"
	"github.com/spf13/cobra"
)

// newInitCommand scaffolds a project.
func newInitCommand(g *globals, stdout, stderr io.Writer) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Scaffold forge.yaml and an example pipeline",
		Long: `Create a forge.yaml with commented defaults, an example pipeline, and the
.forge state directory. Existing files are left alone unless --force is given.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir := g.dir
			if dir == "" {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				dir = wd
			}
			root, err := filepath.Abs(dir)
			if err != nil {
				return err
			}

			cfg := config.Default(root)
			if err := cfg.EnsureDirs(); err != nil {
				return err
			}

			created := []string{}
			skipped := []string{}

			write := func(path, content string, mode os.FileMode) error {
				if _, err := os.Stat(path); err == nil && !force {
					skipped = append(skipped, relTo(root, path))
					return nil
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					return err
				}
				if err := os.WriteFile(path, []byte(content), mode); err != nil {
					return fmt.Errorf("write %s: %w", path, err)
				}
				created = append(created, relTo(root, path))
				return nil
			}

			if err := write(filepath.Join(root, config.DefaultFileName), defaultConfigYAML, 0o644); err != nil {
				return err
			}
			if err := write(filepath.Join(root, "pipeline.yml"), examplePipelineYAML, 0o644); err != nil {
				return err
			}
			// Secrets are sensitive, so the template is created unreadable by
			// anyone else and the store refuses to load it if that changes.
			if err := write(cfg.Secrets.File, secretsTemplate, 0o600); err != nil {
				return err
			}
			if err := write(filepath.Join(cfg.Home, ".gitignore"), forgeGitignore, 0o644); err != nil {
				return err
			}

			if g.jsonOutput {
				return writeJSONTo(stdout, map[string]any{
					"root": root, "created": created, "skipped": skipped,
				})
			}

			for _, f := range created {
				success(stdout, "created %s", bold(f))
			}
			for _, f := range skipped {
				warn(stdout, "%s already exists, left unchanged", f)
			}
			if len(created) == 0 && len(skipped) > 0 && !force {
				hint(stdout, "\nUse --force to overwrite existing files.")
			}
			fmt.Fprintln(stdout)
			hint(stdout, "Next:")
			hint(stdout, "  forge validate pipeline.yml   check the pipeline")
			hint(stdout, "  forge run pipeline.yml        run it")
			hint(stdout, "  forge serve                   open the dashboard")
			return nil
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "overwrite existing files")
	return cmd
}

// newValidateCommand parses and checks a pipeline without running it.
func newValidateCommand(g *globals, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "validate [pipeline.yml...]",
		Short: "Check a pipeline for errors without running it",
		Long: `Parse one or more pipeline files, report every problem found, and print the
resolved execution plan. Nothing is executed and no state is written.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				args = []string{"pipeline.yml"}
			}

			type result struct {
				Path    string   `json:"path"`
				Valid   bool     `json:"valid"`
				Name    string   `json:"name,omitempty"`
				Jobs    int      `json:"jobs,omitempty"`
				Stages  []string `json:"stages,omitempty"`
				Order   []string `json:"order,omitempty"`
				Problem string   `json:"problem,omitempty"`
			}

			results := make([]result, 0, len(args))
			failed := false

			for _, path := range args {
				spec, graph, err := pipeline.Load(path)
				if err != nil {
					failed = true
					results = append(results, result{Path: path, Valid: false, Problem: err.Error()})
					continue
				}
				results = append(results, result{
					Path: path, Valid: true, Name: spec.Name,
					Jobs: len(spec.Jobs), Stages: graph.Stages, Order: graph.Order,
				})
			}

			if g.jsonOutput {
				if err := writeJSONTo(stdout, map[string]any{"results": results}); err != nil {
					return err
				}
				if failed {
					return silentError{code: 1}
				}
				return nil
			}

			for i, res := range results {
				if i > 0 {
					fmt.Fprintln(stdout)
				}
				if !res.Valid {
					fail(stdout, "%s", bold(res.Path))
					for _, line := range strings.Split(res.Problem, "\n") {
						fmt.Fprintf(stdout, "  %s\n", line)
					}
					continue
				}

				success(stdout, "%s — pipeline %s, %d job(s)", bold(res.Path), bold(res.Name), res.Jobs)
				spec, graph, err := pipeline.Load(res.Path)
				if err != nil {
					continue
				}
				printPlan(stdout, spec, graph)
			}

			if failed {
				return silentError{code: 1}
			}
			return nil
		},
	}
}

// printPlan renders the resolved execution plan: which jobs run in parallel and
// what each one waits for. This is the payoff of `validate` — it answers "what
// will actually happen" without running anything.
func printPlan(w io.Writer, spec *pipeline.Spec, graph *pipeline.Graph) {
	layers := graph.Layers()
	fmt.Fprintf(w, "\n  %s\n", dim("execution plan"))
	for i, layer := range layers {
		label := fmt.Sprintf("  %s", dim(fmt.Sprintf("wave %d", i+1)))
		if len(layer) > 1 {
			label += dim(fmt.Sprintf(" (%d in parallel)", len(layer)))
		}
		fmt.Fprintln(w, label)
		for _, name := range layer {
			node := graph.Nodes[name]
			job := spec.Jobs[name]

			var notes []string
			if len(node.Needs) > 0 {
				verb := "needs"
				if node.Implicit {
					verb = "after stage"
				}
				notes = append(notes, fmt.Sprintf("%s %s", verb, strings.Join(node.Needs, ", ")))
			}
			if job.When != pipeline.WhenOnSuccess {
				notes = append(notes, "when: "+string(job.When))
			}
			if job.If != "" {
				notes = append(notes, "if: "+job.If)
			}
			if job.Image != "" {
				notes = append(notes, "image: "+job.Image)
			}
			if job.Retry.Max > 0 {
				notes = append(notes, fmt.Sprintf("retry: %d", job.Retry.Max))
			}
			if job.AllowFailure {
				notes = append(notes, "allow_failure")
			}
			if job.Artifacts != nil && len(job.Artifacts.Paths) > 0 {
				notes = append(notes, "artifacts: "+strings.Join(job.Artifacts.Paths, ", "))
			}

			line := fmt.Sprintf("    %s %s", dim("•"), name)
			if job.Stage != pipeline.DefaultStage {
				line += dim(fmt.Sprintf(" [%s]", job.Stage))
			}
			fmt.Fprintln(w, line)
			if len(notes) > 0 {
				fmt.Fprintf(w, "      %s\n", dim(strings.Join(notes, "  ·  ")))
			}
		}
	}
}

func relTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}

// resolvePipelinePath finds the pipeline file to use, defaulting to the
// conventional names in the project root.
func resolvePipelinePath(arg, root string) (string, error) {
	if arg != "" {
		if _, err := os.Stat(arg); err != nil {
			return "", fmt.Errorf("pipeline %s: %w", arg, err)
		}
		return arg, nil
	}
	for _, candidate := range []string{"pipeline.yml", "pipeline.yaml", ".forge/pipeline.yml"} {
		full := filepath.Join(root, candidate)
		if _, err := os.Stat(full); err == nil {
			return full, nil
		}
	}
	return "", errors.New("no pipeline file given and none of pipeline.yml, pipeline.yaml exists here")
}

const defaultConfigYAML = `# forge configuration.
#
# Every setting here has a working default, so this file is optional and any
# key may be removed. Paths are relative to the .forge home directory.

runner:
  # Maximum jobs executing at once. Defaults to the number of CPUs.
  concurrency: 4
  # "local" runs commands as child processes; "docker" runs them in containers.
  # A job that declares an image uses docker regardless of this setting.
  default_executor: local
  # Wall-clock limit applied to jobs that do not set their own timeout.
  default_timeout: 30m
  # Extra attempts for jobs that do not set their own retry budget.
  default_retries: 0
  retry_backoff: 2s
  # Image used by docker jobs that do not name one.
  docker_image: alpine:3.20
  # Host environment variables jobs are allowed to inherit. Anything not listed
  # is withheld, so a job cannot read the rest of your shell environment.
  env_passthrough: [PATH, HOME, LANG, LC_ALL, TZ, TERM, USER, SHELL, TMPDIR]
  # How long a manual job waits for approval before being skipped. 0 waits
  # indefinitely (dashboard runs); ` + "`forge run`" + ` skips manual jobs by default.
  manual_timeout: 0s

workspace:
  # "isolated" gives every job its own copy of the project, seeded with the
  # artifacts of the jobs it needs. "shared" reuses one directory per run,
  # which is faster on large trees but unsafe for concurrent jobs.
  mode: isolated
  ignore: [.git, .forge, node_modules, .venv, __pycache__]
  # Run directories kept on disk. 0 keeps everything.
  keep_runs: 50

artifacts:
  dir: artifacts
  retention: 720h # 30 days; 0 keeps forever
  max_file_size: 268435456 # 256 MiB
  max_total_size: 1073741824 # 1 GiB per job

logs:
  dir: logs
  max_size: 16777216 # 16 MiB per stream per attempt
  retention: 720h

server:
  # forge can start pipelines, which means it can run shell commands. Binding
  # beyond loopback requires server.token to be set.
  host: 127.0.0.1
  port: 7777
  # token: ""

secrets:
  # KEY=VALUE file, relative to the forge home. Never committed, never written
  # to the database, and every value is masked in captured logs.
  file: secrets.env
  # Host environment variables to treat as secrets.
  env: []
`

const examplePipelineYAML = `name: example

variables:
  NODE_ENV: test

stages:
  - build
  - test
  - package

jobs:
  build:
    stage: build
    commands:
      - echo "Building"
      - mkdir -p dist
      - echo "hello" > dist/output.txt
    artifacts:
      paths:
        - dist/

  test:
    stage: test
    needs:
      - build
    commands:
      - echo "Running tests"
      # Artifacts from ` + "`needs`" + ` are restored into this job's workspace.
      - cat dist/output.txt

  package:
    stage: package
    needs:
      - test
    commands:
      - echo "Packaging"
`

//nolint:gosec // G101: this is an empty commented template, not a credential.
const secretsTemplate = `# Secrets for forge jobs, as KEY=VALUE lines.
#
# These are injected into job environments and masked wherever they appear in
# captured output. They are never written to the database.
#
# This file must not be committed and must stay mode 0600.
#
# EXAMPLE_TOKEN=replace-me
`

const forgeGitignore = `# forge state: local to this machine, never committed.
*
`
