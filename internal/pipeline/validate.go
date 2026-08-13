package pipeline

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// ValidationError collects every problem found in a pipeline so the author sees
// all of them at once instead of fixing one, re-running, and finding the next.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	switch len(e.Problems) {
	case 0:
		return "pipeline is invalid"
	case 1:
		return e.Problems[0]
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%d problems:", len(e.Problems))
		for _, p := range e.Problems {
			b.WriteString("\n  - ")
			b.WriteString(p)
		}
		return b.String()
	}
}

// Validate checks the document for problems that would make execution ambiguous
// or unsafe. It is deliberately strict about ordering and references, since those
// are the mistakes that turn into confusing runtime behaviour.
func (s *Spec) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if normalizeName(s.Name) == "" {
		add("pipeline `name` is required")
	}
	if len(s.Jobs) == 0 {
		add("pipeline must define at least one job under `jobs`")
	}

	// Stage declarations must be unique, and a job may only use a declared stage.
	declared := make(map[string]bool, len(s.Stages))
	for _, st := range s.Stages {
		name := normalizeName(st)
		if name == "" {
			add("`stages` contains an empty entry")
			continue
		}
		if declared[name] {
			add("stage %q is declared more than once", name)
		}
		declared[name] = true
	}

	for _, name := range sortedKeys(s.Jobs) {
		job := s.Jobs[name]
		if normalizeName(name) == "" {
			add("a job has an empty name")
			continue
		}

		if job.Stage != DefaultStage && !declared[job.Stage] {
			add("job %q uses stage %q, which is not listed in `stages`", name, job.Stage)
		}
		if job.Stage == DefaultStage && len(s.Stages) > 0 && !declared[DefaultStage] {
			add("job %q declares no `stage`, but the pipeline declares `stages`; add a stage to the job", name)
		}

		if len(job.Commands) == 0 && job.When != WhenNever {
			add("job %q has no `commands`", name)
		}
		for i, cmd := range job.Commands {
			if strings.TrimSpace(cmd) == "" {
				add("job %q command %d is empty", name, i+1)
			}
		}

		for _, need := range job.Needs {
			need = normalizeName(need)
			if need == "" {
				add("job %q has an empty entry in `needs`", name)
				continue
			}
			if need == name {
				add("job %q lists itself in `needs`", name)
				continue
			}
			if _, ok := s.Jobs[need]; !ok {
				add("job %q needs %q, which is not defined", name, need)
			}
		}
		if dupes := duplicates(job.Needs); len(dupes) > 0 {
			add("job %q lists %s more than once in `needs`", name, quoteList(dupes))
		}

		if !job.When.Valid() {
			add("job %q has invalid `when` %q (want on_success, on_failure, always, manual or never)", name, job.When)
		}
		switch job.Executor {
		case "", "local", "docker":
		default:
			add("job %q has invalid `executor` %q (want local or docker)", name, job.Executor)
		}
		if job.Executor == "local" && job.Image != "" {
			add("job %q sets `image` but forces `executor: local`; the image would be ignored", name)
		}
		if job.Timeout < 0 {
			add("job %q has a negative `timeout`", name)
		}
		if job.Retry.Max < 0 {
			add("job %q has a negative `retry.max`", name)
		}
		if job.Retry.Backoff < 0 {
			add("job %q has a negative `retry.backoff`", name)
		}

		// working_dir must stay inside the job workspace. Rejecting it here means
		// the executor never has to reason about escaping paths.
		if job.WorkingDir != "" {
			if filepath.IsAbs(job.WorkingDir) {
				add("job %q `working_dir` must be relative to the workspace, got %q", name, job.WorkingDir)
			} else if escapes(job.WorkingDir) {
				add("job %q `working_dir` %q escapes the workspace", name, job.WorkingDir)
			}
		}

		if job.If != "" {
			if _, err := ParseCondition(job.If); err != nil {
				add("job %q has an invalid `if` condition: %v", name, err)
			}
		}

		if job.Artifacts != nil {
			problems = append(problems, validateArtifacts(name, job.Artifacts)...)
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return &ValidationError{Problems: problems}
	}

	// Structural checks need a well-formed job set, so they run only once the
	// per-job checks above have passed.
	if _, err := s.Graph(); err != nil {
		return &ValidationError{Problems: []string{err.Error()}}
	}
	return nil
}

// validateArtifacts checks an artifact selector. Path traversal is rejected at
// parse time as well as at collection time: defence in depth, and a much better
// error message than a runtime failure halfway through a run.
func validateArtifacts(jobName string, a *Artifacts) []string {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if len(a.Paths) == 0 {
		add("job %q declares `artifacts` with no `paths`", jobName)
	}
	if !a.When.Valid() {
		add("job %q has invalid `artifacts.when` %q (want on_success, on_failure or always)", jobName, a.When)
	}
	if a.ExpireIn < 0 {
		add("job %q has a negative `artifacts.expire_in`", jobName)
	}
	for _, p := range append(append([]string{}, a.Paths...), a.Exclude...) {
		switch {
		case strings.TrimSpace(p) == "":
			add("job %q has an empty artifact path", jobName)
		case filepath.IsAbs(p) || strings.HasPrefix(p, "/"):
			add("job %q artifact path %q must be relative to the workspace", jobName, p)
		case escapes(p):
			add("job %q artifact path %q escapes the workspace", jobName, p)
		}
	}
	return problems
}

// escapes reports whether a relative path climbs out of its base directory.
func escapes(p string) bool {
	cleaned := filepath.ToSlash(filepath.Clean(p))
	return cleaned == ".." || strings.HasPrefix(cleaned, "../")
}

func sortedKeys(m map[string]*Job) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func duplicates(items []string) []string {
	seen := make(map[string]int, len(items))
	for _, it := range items {
		seen[it]++
	}
	var out []string
	for it, n := range seen {
		if n > 1 {
			out = append(out, it)
		}
	}
	sort.Strings(out)
	return out
}

func quoteList(items []string) string {
	quoted := make([]string, len(items))
	for i, it := range items {
		quoted[i] = fmt.Sprintf("%q", it)
	}
	return strings.Join(quoted, ", ")
}
