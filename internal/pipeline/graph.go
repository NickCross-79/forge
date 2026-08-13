package pipeline

import (
	"fmt"
	"sort"
	"strings"
)

// Node is one job in the resolved dependency graph.
type Node struct {
	Job *Job
	// Needs is the resolved dependency set: explicit `needs` when given, otherwise
	// the implicit dependency on the previous stage.
	Needs []string
	// Dependents is the reverse edge, used to propagate failures and to release
	// downstream work as jobs finish.
	Dependents []string
	// Implicit records that Needs came from stage ordering rather than from the
	// job's own `needs`, which the dashboard renders differently.
	Implicit bool
}

// Graph is the validated, acyclic job graph for a pipeline.
type Graph struct {
	Nodes map[string]*Node
	// Order is a deterministic topological ordering. Jobs are safe to start in
	// this order, though the scheduler runs independent ones concurrently.
	Order []string
	// Stages is the effective stage list, including the implicit default stage.
	Stages []string
}

// CycleError reports a dependency cycle, naming the jobs involved so the author
// can find it without re-reading the whole file.
type CycleError struct {
	Cycle []string
}

func (e *CycleError) Error() string {
	if len(e.Cycle) == 0 {
		return "dependency cycle detected"
	}
	return "dependency cycle detected: " + strings.Join(e.Cycle, " -> ")
}

// Graph resolves dependencies and verifies the result is acyclic.
//
// A job with explicit `needs` depends on exactly those jobs. A job without
// `needs` depends on every job in the nearest preceding stage that has any, which
// is what makes `stages` meaningful on its own.
func (s *Spec) Graph() (*Graph, error) {
	stages := s.effectiveStages()
	byStage := make(map[string][]string, len(stages))
	for name, job := range s.Jobs {
		byStage[job.Stage] = append(byStage[job.Stage], name)
	}
	for st := range byStage {
		sort.Strings(byStage[st])
	}

	g := &Graph{Nodes: make(map[string]*Node, len(s.Jobs)), Stages: stages}
	for name, job := range s.Jobs {
		node := &Node{Job: job}
		if len(job.Needs) > 0 {
			node.Needs = dedupe(job.Needs)
		} else {
			node.Needs = previousStageJobs(stages, byStage, job.Stage)
			node.Implicit = len(node.Needs) > 0
		}
		g.Nodes[name] = node
	}

	// A `needs` entry pointing at a missing job is caught by Validate, but Graph
	// is callable on its own so it must not build a graph with dangling edges.
	for name, node := range g.Nodes {
		for _, need := range node.Needs {
			if _, ok := g.Nodes[need]; !ok {
				return nil, fmt.Errorf("job %q needs %q, which is not defined", name, need)
			}
		}
	}

	for name, node := range g.Nodes {
		for _, need := range node.Needs {
			g.Nodes[need].Dependents = append(g.Nodes[need].Dependents, name)
		}
	}
	for _, node := range g.Nodes {
		sort.Strings(node.Dependents)
	}

	order, err := g.topoSort(s)
	if err != nil {
		return nil, err
	}
	g.Order = order
	return g, nil
}

// effectiveStages returns the declared stages, or a single default stage when the
// pipeline does not declare any.
func (s *Spec) effectiveStages() []string {
	if len(s.Stages) > 0 {
		return append([]string(nil), s.Stages...)
	}
	return []string{DefaultStage}
}

// previousStageJobs finds the jobs of the closest earlier stage that has any.
// Skipping over empty stages means an unused stage in the list does not sever
// the ordering between the stages around it.
func previousStageJobs(stages []string, byStage map[string][]string, stage string) []string {
	idx := -1
	for i, st := range stages {
		if st == stage {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return nil
	}
	for i := idx - 1; i >= 0; i-- {
		if jobs := byStage[stages[i]]; len(jobs) > 0 {
			return append([]string(nil), jobs...)
		}
	}
	return nil
}

// topoSort runs Kahn's algorithm. Ready jobs are taken in the spec's stable job
// order so that the resulting sequence is deterministic across runs, and any
// nodes left over at the end are exactly the ones caught in a cycle.
func (g *Graph) topoSort(s *Spec) ([]string, error) {
	indegree := make(map[string]int, len(g.Nodes))
	for name, node := range g.Nodes {
		indegree[name] = len(node.Needs)
	}

	stable := s.JobNames()
	ready := make([]string, 0, len(g.Nodes))
	for _, name := range stable {
		if indegree[name] == 0 {
			ready = append(ready, name)
		}
	}

	order := make([]string, 0, len(g.Nodes))
	for len(ready) > 0 {
		name := ready[0]
		ready = ready[1:]
		order = append(order, name)

		var released []string
		for _, dep := range g.Nodes[name].Dependents {
			indegree[dep]--
			if indegree[dep] == 0 {
				released = append(released, dep)
			}
		}
		sort.Strings(released)
		ready = append(ready, released...)
	}

	if len(order) != len(g.Nodes) {
		remaining := make([]string, 0, len(g.Nodes)-len(order))
		for name := range g.Nodes {
			if indegree[name] > 0 {
				remaining = append(remaining, name)
			}
		}
		sort.Strings(remaining)
		return nil, &CycleError{Cycle: g.findCycle(remaining)}
	}
	return order, nil
}

// findCycle walks the leftover nodes to produce a concrete cycle for the error
// message. Reporting "a -> b -> a" is far more actionable than a list of names.
func (g *Graph) findCycle(candidates []string) []string {
	inCycle := make(map[string]bool, len(candidates))
	for _, name := range candidates {
		inCycle[name] = true
	}

	var (
		path    []string
		onPath  = make(map[string]bool)
		visited = make(map[string]bool)
		cycle   []string
	)

	var walk func(string) bool
	walk = func(name string) bool {
		visited[name] = true
		onPath[name] = true
		path = append(path, name)

		for _, need := range g.Nodes[name].Needs {
			if !inCycle[need] {
				continue
			}
			if onPath[need] {
				// Trim the prefix that leads into the cycle but is not part of it.
				start := 0
				for i, p := range path {
					if p == need {
						start = i
						break
					}
				}
				cycle = append(append([]string(nil), path[start:]...), need)
				return true
			}
			if !visited[need] && walk(need) {
				return true
			}
		}

		path = path[:len(path)-1]
		onPath[name] = false
		return false
	}

	for _, name := range candidates {
		if !visited[name] && walk(name) {
			return cycle
		}
	}
	return candidates
}

// Roots returns jobs with no dependencies, which the scheduler can start at once.
func (g *Graph) Roots() []string {
	var roots []string
	for name, node := range g.Nodes {
		if len(node.Needs) == 0 {
			roots = append(roots, name)
		}
	}
	sort.Strings(roots)
	return roots
}

// TransitiveDependents returns every job reachable downstream of name, which is
// the set to skip when a job fails.
func (g *Graph) TransitiveDependents(name string) []string {
	seen := make(map[string]bool)
	var walk func(string)
	walk = func(n string) {
		node, ok := g.Nodes[n]
		if !ok {
			return
		}
		for _, dep := range node.Dependents {
			if seen[dep] {
				continue
			}
			seen[dep] = true
			walk(dep)
		}
	}
	walk(name)

	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Layers groups jobs into levels where every job in a level depends only on
// earlier levels. The dashboard uses this to lay out the DAG left to right.
func (g *Graph) Layers() [][]string {
	depth := make(map[string]int, len(g.Nodes))
	for _, name := range g.Order {
		d := 0
		for _, need := range g.Nodes[name].Needs {
			if nd := depth[need] + 1; nd > d {
				d = nd
			}
		}
		depth[name] = d
	}

	maxDepth := -1
	for _, d := range depth {
		if d > maxDepth {
			maxDepth = d
		}
	}
	layers := make([][]string, maxDepth+1)
	for _, name := range g.Order {
		layers[depth[name]] = append(layers[depth[name]], name)
	}
	return layers
}

func dedupe(items []string) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, it := range items {
		if seen[it] {
			continue
		}
		seen[it] = true
		out = append(out, it)
	}
	return out
}
