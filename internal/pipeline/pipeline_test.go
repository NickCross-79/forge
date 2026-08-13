package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const exampleYAML = `
name: example

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

  package:
    stage: package
    needs:
      - test
    commands:
      - echo "Packaging"
`

func TestParseExample(t *testing.T) {
	spec, err := Parse([]byte(exampleYAML))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if spec.Name != "example" {
		t.Errorf("Name = %q, want %q", spec.Name, "example")
	}
	if got := spec.Variables["NODE_ENV"]; got != "test" {
		t.Errorf("Variables[NODE_ENV] = %q, want %q", got, "test")
	}
	if len(spec.Jobs) != 3 {
		t.Fatalf("len(Jobs) = %d, want 3", len(spec.Jobs))
	}
	if spec.Jobs["build"].Name != "build" {
		t.Errorf("job name not populated from map key: %q", spec.Jobs["build"].Name)
	}
	if got := spec.Jobs["build"].When; got != WhenOnSuccess {
		t.Errorf("default When = %q, want %q", got, WhenOnSuccess)
	}
	if got := spec.Jobs["build"].Artifacts.When; got != ArtifactOnSuccess {
		t.Errorf("default artifacts.When = %q, want %q", got, ArtifactOnSuccess)
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte("name: x\njobs:\n  a:\n    commands: [echo hi]\n    tpyo: true\n"))
	if err == nil {
		t.Fatal("Parse() succeeded on an unknown field; strict decoding should reject it")
	}
	if !strings.Contains(err.Error(), "tpyo") {
		t.Errorf("error should name the offending field, got %v", err)
	}
}

func TestParseRetryBothForms(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		wantMax     int
		wantBackoff time.Duration
	}{
		{"scalar", "retry: 3", 3, 0},
		{"mapping", "retry:\n      max: 2\n      backoff: 5s", 2, 5 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := "name: p\njobs:\n  a:\n    commands: [echo hi]\n    " + tc.yaml + "\n"
			spec, err := Parse([]byte(doc))
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			job := spec.Jobs["a"]
			if job.Retry.Max != tc.wantMax {
				t.Errorf("Retry.Max = %d, want %d", job.Retry.Max, tc.wantMax)
			}
			if job.Retry.Backoff != tc.wantBackoff {
				t.Errorf("Retry.Backoff = %v, want %v", job.Retry.Backoff, tc.wantBackoff)
			}
		})
	}
}

func TestApplyDefaults(t *testing.T) {
	doc := `
name: p
defaults:
  image: node:20
  timeout: 5m
  retry:
    max: 2
    backoff: 1s
  env:
    FROM_DEFAULT: "1"
jobs:
  inherits:
    commands: [echo hi]
  overrides:
    image: python:3.12
    timeout: 30s
    commands: [echo hi]
`
	spec, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	inherits := spec.Jobs["inherits"]
	if inherits.Image != "node:20" {
		t.Errorf("Image = %q, want inherited node:20", inherits.Image)
	}
	if inherits.Timeout != 5*time.Minute {
		t.Errorf("Timeout = %v, want 5m", inherits.Timeout)
	}
	if inherits.Retry.Max != 2 {
		t.Errorf("Retry.Max = %d, want 2", inherits.Retry.Max)
	}
	// An image with no explicit executor implies docker.
	if inherits.Executor != "docker" {
		t.Errorf("Executor = %q, want docker (implied by image)", inherits.Executor)
	}

	overrides := spec.Jobs["overrides"]
	if overrides.Image != "python:3.12" {
		t.Errorf("Image = %q, want python:3.12", overrides.Image)
	}
	if overrides.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", overrides.Timeout)
	}

	env := spec.Environment(inherits)
	if env["FROM_DEFAULT"] != "1" {
		t.Errorf("defaults.env not merged: %v", env)
	}
}

func TestEnvironmentPrecedence(t *testing.T) {
	doc := `
name: p
variables:
  V: pipeline
  ONLY_PIPELINE: yes
defaults:
  env:
    V: defaults
jobs:
  a:
    commands: [echo hi]
    env:
      V: job-env
  b:
    commands: [echo hi]
    env:
      V: job-env
    variables:
      V: job-vars
`
	spec, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got := spec.Environment(spec.Jobs["a"])["V"]; got != "job-env" {
		t.Errorf("job env should beat defaults and pipeline vars, got %q", got)
	}
	if got := spec.Environment(spec.Jobs["b"])["V"]; got != "job-vars" {
		t.Errorf("job variables should beat job env, got %q", got)
	}
	if got := spec.Environment(spec.Jobs["a"])["ONLY_PIPELINE"]; got != "yes" {
		t.Errorf("pipeline variables should be present, got %q", got)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	doc := `
name: ""
stages: [build, build]
jobs:
  a:
    stage: nope
    commands: []
    needs: [missing]
  b:
    commands: [echo hi]
    when: whenever
    executor: podman
`
	spec, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	err = spec.Validate()
	if err == nil {
		t.Fatal("Validate() returned nil for an invalid document")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error type = %T, want *ValidationError", err)
	}

	joined := strings.Join(ve.Problems, "\n")
	for _, want := range []string{
		"`name` is required",
		`stage "build" is declared more than once`,
		`job "a" uses stage "nope"`,
		`job "a" has no `,
		`job "a" needs "missing"`,
		`job "b" has invalid ` + "`when`",
		`job "b" has invalid ` + "`executor`",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems missing %q:\n%s", want, joined)
		}
	}
}

func TestValidateRejectsSelfDependency(t *testing.T) {
	spec := mustParse(t, "name: p\njobs:\n  a:\n    commands: [echo hi]\n    needs: [a]\n")
	err := spec.Validate()
	if err == nil || !strings.Contains(err.Error(), "lists itself") {
		t.Fatalf("want self-dependency error, got %v", err)
	}
}

func TestValidateRejectsEscapingPaths(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "working_dir escapes",
			doc:  "name: p\njobs:\n  a:\n    commands: [echo hi]\n    working_dir: ../outside\n",
			want: "escapes the workspace",
		},
		{
			name: "working_dir absolute",
			doc:  "name: p\njobs:\n  a:\n    commands: [echo hi]\n    working_dir: /etc\n",
			want: "must be relative",
		},
		{
			name: "artifact path escapes",
			doc:  "name: p\njobs:\n  a:\n    commands: [echo hi]\n    artifacts:\n      paths: ['../../etc/passwd']\n",
			want: "escapes the workspace",
		},
		{
			name: "artifact path absolute",
			doc:  "name: p\njobs:\n  a:\n    commands: [echo hi]\n    artifacts:\n      paths: ['/etc/passwd']\n",
			want: "must be relative",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := mustParse(t, tc.doc)
			err := spec.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestGraphExplicitNeeds(t *testing.T) {
	spec := mustParse(t, exampleYAML)
	g, err := spec.Graph()
	if err != nil {
		t.Fatalf("Graph() error = %v", err)
	}
	want := []string{"build", "test", "package"}
	if got := g.Order; !equalSlices(got, want) {
		t.Errorf("Order = %v, want %v", got, want)
	}
	if got := g.Roots(); !equalSlices(got, []string{"build"}) {
		t.Errorf("Roots() = %v, want [build]", got)
	}
	if got := g.Nodes["test"].Needs; !equalSlices(got, []string{"build"}) {
		t.Errorf("test.Needs = %v, want [build]", got)
	}
	if got := g.Nodes["build"].Dependents; !equalSlices(got, []string{"test"}) {
		t.Errorf("build.Dependents = %v, want [test]", got)
	}
	if got := g.TransitiveDependents("build"); !equalSlices(got, []string{"package", "test"}) {
		t.Errorf("TransitiveDependents(build) = %v, want [package test]", got)
	}
}

func TestGraphImplicitStageDependencies(t *testing.T) {
	// No `needs` anywhere: ordering must come from stages alone.
	doc := `
name: p
stages: [build, test, deploy]
jobs:
  compile:
    stage: build
    commands: [echo hi]
  unit:
    stage: test
    commands: [echo hi]
  lint:
    stage: test
    commands: [echo hi]
  ship:
    stage: deploy
    commands: [echo hi]
`
	spec := mustParse(t, doc)
	g, err := spec.Graph()
	if err != nil {
		t.Fatalf("Graph() error = %v", err)
	}
	if got := g.Nodes["unit"].Needs; !equalSlices(got, []string{"compile"}) {
		t.Errorf("unit.Needs = %v, want [compile]", got)
	}
	if !g.Nodes["unit"].Implicit {
		t.Error("unit.Implicit = false, want true for stage-derived dependencies")
	}
	if got := g.Nodes["ship"].Needs; !equalSlices(got, []string{"lint", "unit"}) {
		t.Errorf("ship.Needs = %v, want [lint unit]", got)
	}
	// lint and unit are independent, so they belong to the same layer.
	layers := g.Layers()
	if len(layers) != 3 {
		t.Fatalf("len(Layers) = %d, want 3", len(layers))
	}
	if !equalSlices(layers[1], []string{"lint", "unit"}) {
		t.Errorf("layers[1] = %v, want [lint unit]", layers[1])
	}
}

func TestGraphSkipsEmptyStages(t *testing.T) {
	// The unused `test` stage must not sever ordering between build and deploy.
	doc := `
name: p
stages: [build, test, deploy]
jobs:
  compile:
    stage: build
    commands: [echo hi]
  ship:
    stage: deploy
    commands: [echo hi]
`
	g, err := mustParse(t, doc).Graph()
	if err != nil {
		t.Fatalf("Graph() error = %v", err)
	}
	if got := g.Nodes["ship"].Needs; !equalSlices(got, []string{"compile"}) {
		t.Errorf("ship.Needs = %v, want [compile] across the empty stage", got)
	}
}

func TestGraphDetectsCycles(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want []string
	}{
		{
			name: "two job cycle",
			doc: `
name: p
jobs:
  a:
    commands: [echo hi]
    needs: [b]
  b:
    commands: [echo hi]
    needs: [a]
`,
			want: []string{"a", "b"},
		},
		{
			name: "three job cycle",
			doc: `
name: p
jobs:
  a:
    commands: [echo hi]
    needs: [c]
  b:
    commands: [echo hi]
    needs: [a]
  c:
    commands: [echo hi]
    needs: [b]
`,
			want: []string{"a", "b", "c"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := mustParse(t, tc.doc)
			_, err := spec.Graph()
			if err == nil {
				t.Fatal("Graph() returned nil error for a cyclic pipeline")
			}
			var ce *CycleError
			if !errors.As(err, &ce) {
				t.Fatalf("error type = %T, want *CycleError", err)
			}
			for _, name := range tc.want {
				if !strings.Contains(ce.Error(), name) {
					t.Errorf("cycle error should mention %q, got %q", name, ce.Error())
				}
			}
			// Validate must surface the cycle too, not just Graph.
			if err := spec.Validate(); err == nil || !strings.Contains(err.Error(), "cycle") {
				t.Errorf("Validate() should report the cycle, got %v", err)
			}
		})
	}
}

func TestGraphCycleDoesNotIncludeAcyclicPrefix(t *testing.T) {
	// `root` feeds the cycle but is not part of it, so it must not be reported.
	doc := `
name: p
jobs:
  root:
    commands: [echo hi]
  a:
    commands: [echo hi]
    needs: [root, c]
  b:
    commands: [echo hi]
    needs: [a]
  c:
    commands: [echo hi]
    needs: [b]
`
	_, err := mustParse(t, doc).Graph()
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("want *CycleError, got %v", err)
	}
	for _, name := range ce.Cycle {
		if name == "root" {
			t.Errorf("cycle %v should not include the acyclic prefix job", ce.Cycle)
		}
	}
}

func TestJobNamesIsDeterministic(t *testing.T) {
	spec := mustParse(t, exampleYAML)
	first := spec.JobNames()
	for i := 0; i < 50; i++ {
		if got := spec.JobNames(); !equalSlices(got, first) {
			t.Fatalf("JobNames() is not stable: %v then %v", first, got)
		}
	}
	if !equalSlices(first, []string{"build", "test", "package"}) {
		t.Errorf("JobNames() = %v, want stage order [build test package]", first)
	}
}

func TestCloneIsDeep(t *testing.T) {
	spec := mustParse(t, exampleYAML)
	clone := spec.Clone()
	clone.Variables["NODE_ENV"] = "mutated"
	clone.Jobs["build"].Commands[0] = "mutated"
	clone.Jobs["build"].Artifacts.Paths[0] = "mutated"

	if spec.Variables["NODE_ENV"] != "test" {
		t.Error("Clone shares the variables map")
	}
	if spec.Jobs["build"].Commands[0] == "mutated" {
		t.Error("Clone shares the commands slice")
	}
	if spec.Jobs["build"].Artifacts.Paths[0] == "mutated" {
		t.Error("Clone shares the artifacts struct")
	}
}

func TestParseFileAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")
	if err := os.WriteFile(path, []byte(exampleYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, g, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if spec.Name != "example" || len(g.Order) != 3 {
		t.Errorf("Load() returned unexpected spec/graph: %q %v", spec.Name, g.Order)
	}

	if _, _, err := Load(filepath.Join(dir, "missing.yml")); err == nil {
		t.Error("Load() should fail on a missing file")
	}
}

func TestChecksumIsStable(t *testing.T) {
	a := Checksum([]byte(exampleYAML))
	b := Checksum([]byte(exampleYAML))
	c := Checksum([]byte(exampleYAML + "\n"))
	if a != b {
		t.Error("Checksum is not deterministic")
	}
	if a == c {
		t.Error("Checksum did not change for different content")
	}
	if len(a) != 64 {
		t.Errorf("Checksum length = %d, want 64 hex chars", len(a))
	}
}

func mustParse(t *testing.T, doc string) *Spec {
	t.Helper()
	spec, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return spec
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
