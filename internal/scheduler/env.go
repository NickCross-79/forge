package scheduler

import (
	"os"
	"strconv"

	"github.com/nickcross-79/forge/internal/pipeline"
)

// jobEnvironment assembles the complete environment a job will see.
//
// Nothing is inherited implicitly. The host environment contributes only the
// variables named in runner.env_passthrough, so a job cannot read the operator's
// tokens and shell configuration just because they happen to be exported. Layers,
// lowest precedence first:
//
//  1. allow-listed host variables
//  2. forge's own FORGE_* / CI variables
//  3. pipeline variables, pipeline defaults env, job env, job variables
//  4. secrets
//
// Secrets are applied last so a pipeline cannot accidentally shadow one with a
// plain variable of the same name, and every secret value is registered with the
// log redactor before the job starts.
func (s *Scheduler) jobEnvironment(rs *runState, job *pipeline.Job, workspace string, attempt int) map[string]string {
	env := make(map[string]string, 32)

	for _, name := range s.cfg.Runner.EnvPassthrough {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}

	env["CI"] = "true"
	env["FORGE"] = "true"
	env["FORGE_PIPELINE"] = rs.spec.Name
	env["FORGE_RUN_ID"] = strconv.FormatInt(rs.run.ID, 10)
	env["FORGE_RUN_NUMBER"] = strconv.FormatInt(rs.run.Number, 10)
	env["FORGE_JOB"] = job.Name
	env["FORGE_STAGE"] = job.Stage
	env["FORGE_ATTEMPT"] = strconv.Itoa(attempt)
	env["FORGE_WORKSPACE"] = workspace
	env["FORGE_PROJECT_DIR"] = s.cfg.Root

	for k, v := range rs.spec.Environment(job) {
		env[k] = v
	}

	for k, v := range s.jobSecrets(job) {
		env[k] = v
	}
	return env
}

// jobSecrets returns the secrets to inject into a job. An empty `secrets:` list
// means all of them; naming them explicitly narrows what the job can read.
func (s *Scheduler) jobSecrets(job *pipeline.Job) map[string]string {
	if s.secrets == nil || s.secrets.Len() == 0 {
		return nil
	}
	out := make(map[string]string)
	if len(job.Secrets) == 0 {
		for _, name := range s.secrets.Names() {
			if v, ok := s.secrets.Get(name); ok {
				out[name] = v
			}
		}
		return out
	}
	for _, name := range job.Secrets {
		if v, ok := s.secrets.Get(name); ok {
			out[name] = v
		}
	}
	return out
}

// conditionEnvironment is the environment an `if` expression is evaluated
// against. It excludes the workspace path and attempt number, which do not exist
// yet when the condition is checked.
func (s *Scheduler) conditionEnvironment(rs *runState, job *pipeline.Job) map[string]string {
	return s.jobEnvironment(rs, job, "", 1)
}
