package pipeline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Parse decodes a pipeline document and applies defaults. It does not validate;
// callers normally want Load, which parses, validates and builds the graph.
//
// Decoding is strict: an unrecognised key is an error rather than being silently
// ignored, because a typo'd key in CI config usually means the pipeline is not
// doing what its author believes it is.
func Parse(data []byte) (*Spec, error) {
	var spec Spec
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("parse pipeline: %w", err)
	}
	if spec.Jobs == nil {
		spec.Jobs = map[string]*Job{}
	}
	for name, job := range spec.Jobs {
		if job == nil {
			// `jobname:` with an empty body decodes to nil; keep the name so
			// validation can report a useful error instead of panicking.
			job = &Job{}
			spec.Jobs[name] = job
		}
		job.Name = name
	}
	spec.applyDefaults()
	return &spec, nil
}

// ParseFile reads and parses a pipeline file.
func ParseFile(path string) (*Spec, error) {
	data, err := os.ReadFile(path) //nolint:gosec // user-supplied pipeline path
	if err != nil {
		return nil, fmt.Errorf("read pipeline: %w", err)
	}
	spec, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return spec, nil
}

// Load parses, validates and builds the job graph for a pipeline file. This is
// the entry point used by `forge validate` and `forge run`.
func Load(path string) (*Spec, *Graph, error) {
	spec, err := ParseFile(path)
	if err != nil {
		return nil, nil, err
	}
	if err := spec.Validate(); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	graph, err := spec.Graph()
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return spec, graph, nil
}

// applyDefaults folds pipeline-level defaults into each job. Job settings win;
// defaults only fill gaps.
func (s *Spec) applyDefaults() {
	for _, job := range s.Jobs {
		if job.When == "" {
			job.When = WhenOnSuccess
		}
		if job.Stage == "" {
			job.Stage = DefaultStage
		}
		if job.Image == "" {
			job.Image = s.Defaults.Image
		}
		if job.Executor == "" {
			job.Executor = s.Defaults.Executor
		}
		if job.Timeout == 0 {
			job.Timeout = s.Defaults.Timeout
		}
		if job.Retry.Max == 0 {
			job.Retry = s.Defaults.Retry
		}
		if job.Retry.Backoff == 0 {
			job.Retry.Backoff = s.Defaults.Retry.Backoff
		}
		if job.WorkingDir == "" {
			job.WorkingDir = s.Defaults.WorkingDir
		}
		if len(job.Secrets) == 0 {
			job.Secrets = append([]string(nil), s.Defaults.Secrets...)
		}
		if job.Artifacts != nil {
			if job.Artifacts.When == "" {
				job.Artifacts.When = ArtifactOnSuccess
			}
			if job.Artifacts.Name == "" {
				job.Artifacts.Name = job.Name
			}
		}
		// An image without an explicit executor means the author wants a container.
		if job.Executor == "" && job.Image != "" {
			job.Executor = "docker"
		}
	}
}

// Environment returns the environment a job sees, before secrets and forge's own
// FORGE_* variables are layered on. Precedence, lowest first: pipeline variables,
// pipeline default env, job env, job variables.
func (s *Spec) Environment(job *Job) map[string]string {
	env := make(map[string]string, len(s.Variables)+len(job.Env)+len(job.Variables))
	for k, v := range s.Variables {
		env[k] = v
	}
	for k, v := range s.Defaults.Env {
		env[k] = v
	}
	for k, v := range job.Env {
		env[k] = v
	}
	for k, v := range job.Variables {
		env[k] = v
	}
	return env
}

// Checksum is the SHA-256 of a pipeline document, used to notice when a stored
// pipeline's source has changed.
func Checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Marshal renders the spec back to YAML.
func (s *Spec) Marshal() ([]byte, error) {
	return yaml.Marshal(s)
}

// JobNames returns job names in a stable order: by stage as declared, then
// alphabetically within a stage. Map iteration order is random in Go, so every
// listing in the CLI, API and dashboard goes through this.
func (s *Spec) JobNames() []string {
	stageRank := make(map[string]int, len(s.Stages))
	for i, st := range s.Stages {
		stageRank[st] = i
	}
	names := make([]string, 0, len(s.Jobs))
	for name := range s.Jobs {
		names = append(names, name)
	}
	sortStrings(names, func(a, b string) bool {
		ja, jb := s.Jobs[a], s.Jobs[b]
		ra, oka := stageRank[ja.Stage]
		rb, okb := stageRank[jb.Stage]
		switch {
		case oka && okb && ra != rb:
			return ra < rb
		case oka != okb:
			return oka
		default:
			return a < b
		}
	})
	return names
}

// sortStrings is a tiny insertion sort. The job counts involved are small and
// this keeps the comparison closure readable at the call site.
func sortStrings(s []string, less func(a, b string) bool) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && less(s[j], s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// normalizeName trims a job or stage name for comparison in error messages.
func normalizeName(s string) string { return strings.TrimSpace(s) }
