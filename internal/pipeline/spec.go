// Package pipeline defines the forge YAML format and turns a document into a
// validated, cycle-free job graph.
//
// It has no dependencies on the rest of forge: parsing and validation are pure
// functions over a document, which is what makes `forge validate` fast and makes
// the parser easy to test exhaustively.
package pipeline

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// When controls whether a job runs, based on the state of the run so far.
type When string

const (
	// WhenOnSuccess runs the job only if every job it depends on succeeded. Default.
	WhenOnSuccess When = "on_success"
	// WhenOnFailure runs the job only if a previous job in the run failed. Useful
	// for cleanup and notification jobs.
	WhenOnFailure When = "on_failure"
	// WhenAlways runs the job regardless of upstream outcome.
	WhenAlways When = "always"
	// WhenManual holds the job until it is approved via CLI, API or dashboard.
	WhenManual When = "manual"
	// WhenNever disables the job without deleting it from the file.
	WhenNever When = "never"
)

// Valid reports whether w is a recognised value.
func (w When) Valid() bool {
	switch w {
	case WhenOnSuccess, WhenOnFailure, WhenAlways, WhenManual, WhenNever:
		return true
	default:
		return false
	}
}

// ArtifactWhen controls whether artifacts are collected, based on the job outcome.
type ArtifactWhen string

const (
	// ArtifactOnSuccess collects artifacts only from a successful job. Default.
	ArtifactOnSuccess ArtifactWhen = "on_success"
	// ArtifactOnFailure collects only from a failed job, for capturing crash output.
	ArtifactOnFailure ArtifactWhen = "on_failure"
	// ArtifactAlways collects either way.
	ArtifactAlways ArtifactWhen = "always"
)

// Valid reports whether w is a recognised value.
func (w ArtifactWhen) Valid() bool {
	switch w {
	case ArtifactOnSuccess, ArtifactOnFailure, ArtifactAlways:
		return true
	default:
		return false
	}
}

// Spec is a parsed pipeline document.
type Spec struct {
	// Name identifies the pipeline. Runs are grouped and numbered per name.
	Name string `yaml:"name"`
	// Description is shown in the CLI and dashboard.
	Description string `yaml:"description,omitempty"`
	// Variables are environment variables available to every job.
	Variables map[string]string `yaml:"variables,omitempty"`
	// Stages declares execution order for jobs that do not use `needs`.
	Stages []string `yaml:"stages,omitempty"`
	// Defaults supply per-job settings that individual jobs may override.
	Defaults Defaults `yaml:"defaults,omitempty"`
	// Jobs is the work to do, keyed by job name.
	Jobs map[string]*Job `yaml:"jobs"`
}

// Defaults are pipeline-wide job settings.
type Defaults struct {
	Image      string            `yaml:"image,omitempty"`
	Executor   string            `yaml:"executor,omitempty"`
	Timeout    time.Duration     `yaml:"timeout,omitempty"`
	Retry      Retry             `yaml:"retry,omitempty"`
	WorkingDir string            `yaml:"working_dir,omitempty"`
	Env        map[string]string `yaml:"env,omitempty"`
	Secrets    []string          `yaml:"secrets,omitempty"`
}

// Job is one node of the pipeline.
type Job struct {
	// Name is filled in from the map key during parsing; it is not a YAML field.
	Name string `yaml:"-"`
	// Stage places the job in the pipeline's stage ordering.
	Stage string `yaml:"stage,omitempty"`
	// Needs lists jobs that must finish before this one starts. When set, it
	// replaces the implicit ordering that stages would otherwise impose.
	Needs []string `yaml:"needs,omitempty"`
	// Commands are executed in order through a shell. A non-zero exit fails the job.
	Commands []string `yaml:"commands"`
	// Env and Variables are merged into the job environment; both spellings are
	// accepted because pipelines converted from other CI systems use either.
	Env       map[string]string `yaml:"env,omitempty"`
	Variables map[string]string `yaml:"variables,omitempty"`
	// Secrets names the secrets to inject into this job. Empty means all of them.
	Secrets []string `yaml:"secrets,omitempty"`
	// WorkingDir is relative to the job workspace and must stay inside it.
	WorkingDir string `yaml:"working_dir,omitempty"`
	// Timeout bounds the whole job, across all its commands.
	Timeout time.Duration `yaml:"timeout,omitempty"`
	// Retry re-runs the job after a failure.
	Retry Retry `yaml:"retry,omitempty"`
	// Artifacts selects files to keep after the job finishes.
	Artifacts *Artifacts `yaml:"artifacts,omitempty"`
	// Image selects a container image, which implies the docker executor.
	Image string `yaml:"image,omitempty"`
	// Executor forces "local" or "docker".
	Executor string `yaml:"executor,omitempty"`
	// When controls execution based on the outcome of the run so far.
	When When `yaml:"when,omitempty"`
	// If is a condition evaluated against the job environment before running.
	If string `yaml:"if,omitempty"`
	// AllowFailure lets the run continue, and dependents proceed, if this fails.
	AllowFailure bool `yaml:"allow_failure,omitempty"`
}

// Retry describes a job's retry budget. It accepts either a bare count
// (`retry: 2`) or a mapping (`retry: {max: 2, backoff: 5s}`).
type Retry struct {
	// Max is the number of additional attempts after the first one fails.
	Max int `yaml:"max,omitempty"`
	// Backoff is the delay before the first retry; it doubles on each attempt.
	Backoff time.Duration `yaml:"backoff,omitempty"`
}

// UnmarshalYAML accepts both the scalar and mapping forms of `retry`.
func (r *Retry) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var n int
		if err := node.Decode(&n); err != nil {
			return fmt.Errorf("retry: expected a count or a mapping: %w", err)
		}
		r.Max = n
		return nil
	}
	// Alias type avoids recursing into this method.
	type retryAlias Retry
	var alias retryAlias
	if err := node.Decode(&alias); err != nil {
		return err
	}
	*r = Retry(alias)
	return nil
}

// Artifacts selects files to preserve from a job's workspace.
type Artifacts struct {
	// Name labels the artifact set in the UI. Defaults to the job name.
	Name string `yaml:"name,omitempty"`
	// Paths are globs relative to the job workspace. `**` matches across
	// directories, and a trailing `/` collects a directory recursively.
	Paths []string `yaml:"paths"`
	// Exclude removes matches from the selection.
	Exclude []string `yaml:"exclude,omitempty"`
	// When decides collection based on job outcome.
	When ArtifactWhen `yaml:"when,omitempty"`
	// ExpireIn overrides the configured retention for these artifacts.
	ExpireIn time.Duration `yaml:"expire_in,omitempty"`
}

// DefaultStage is used for jobs that declare no stage in a pipeline that
// declares no stages.
const DefaultStage = "default"

// Clone returns a deep copy of the spec, so a run can hold a snapshot that later
// edits to the file cannot mutate.
func (s *Spec) Clone() *Spec {
	if s == nil {
		return nil
	}
	out := *s
	out.Variables = cloneMap(s.Variables)
	out.Stages = append([]string(nil), s.Stages...)
	out.Defaults.Env = cloneMap(s.Defaults.Env)
	out.Defaults.Secrets = append([]string(nil), s.Defaults.Secrets...)
	out.Jobs = make(map[string]*Job, len(s.Jobs))
	for name, job := range s.Jobs {
		out.Jobs[name] = job.Clone()
	}
	return &out
}

// Clone returns a deep copy of the job.
func (j *Job) Clone() *Job {
	if j == nil {
		return nil
	}
	out := *j
	out.Needs = append([]string(nil), j.Needs...)
	out.Commands = append([]string(nil), j.Commands...)
	out.Env = cloneMap(j.Env)
	out.Variables = cloneMap(j.Variables)
	out.Secrets = append([]string(nil), j.Secrets...)
	if j.Artifacts != nil {
		art := *j.Artifacts
		art.Paths = append([]string(nil), j.Artifacts.Paths...)
		art.Exclude = append([]string(nil), j.Artifacts.Exclude...)
		out.Artifacts = &art
	}
	return &out
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
