package scheduler

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/nickcross-79/forge/internal/artifacts"
	"github.com/nickcross-79/forge/internal/config"
)

// prepareWorkspace creates the directory a job will run in.
//
// In isolated mode (the default) each job gets its own copy of the project tree,
// which is what makes concurrent jobs safe: two jobs in the same stage cannot see
// or clobber each other's files. In shared mode every job in the run reuses one
// directory, which is faster on large trees at the cost of that isolation.
func (s *Scheduler) prepareWorkspace(runID int64, jobName string) (string, error) {
	runDir := s.cfg.RunDir(runID)

	var dir string
	if s.cfg.Workspace.Mode == config.WorkspaceShared {
		dir = filepath.Join(runDir, "workspace")
		if _, err := os.Stat(dir); err == nil {
			// Already seeded by an earlier job in this run.
			return dir, nil
		}
	} else {
		dir = filepath.Join(runDir, "jobs", sanitizeDirName(jobName))
		// A retry re-seeds from scratch so a failed attempt cannot leave debris
		// that changes the behaviour of the next one.
		if err := os.RemoveAll(dir); err != nil {
			return "", fmt.Errorf("clear workspace: %w", err)
		}
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create workspace: %w", err)
	}
	if err := copyTree(s.cfg.Root, dir, s.cfg.Workspace.Ignore); err != nil {
		return "", fmt.Errorf("seed workspace from %s: %w", s.cfg.Root, err)
	}
	return dir, nil
}

// copyTree copies src into dst, skipping anything matching an ignore pattern.
//
// Symlinks are recreated as symlinks rather than followed, so a link inside the
// project cannot cause the whole of its target tree to be duplicated into every
// job workspace.
func copyTree(src, dst string, ignore []string) error {
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	dstAbs, err := filepath.Abs(dst)
	if err != nil {
		return err
	}

	return filepath.Walk(srcAbs, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcAbs, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Never copy the destination into itself; the run directory lives under
		// the forge home, which is normally ignored, but not necessarily.
		if artifacts.Contains(path, dstAbs) || path == dstAbs {
			return filepath.SkipDir
		}
		if matchesIgnore(filepath.ToSlash(rel), ignore) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		target := filepath.Join(dstAbs, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, dirPerm(info.Mode().Perm()))
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			// A pre-existing link from a previous attempt would make this fail.
			_ = os.Remove(target)
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copyRegularFile(path, target, info.Mode().Perm())
		default:
			// Sockets, devices and fifos are not project content.
			return nil
		}
	})
}

// matchesIgnore reports whether a workspace-relative path is excluded. A pattern
// matches the path itself or any ancestor directory, so `node_modules` excludes
// everything beneath it without needing `node_modules/**`.
func matchesIgnore(rel string, ignore []string) bool {
	for _, raw := range ignore {
		pattern := artifacts.NormalizePattern(raw)
		if pattern == "" {
			continue
		}
		pattern = strings.TrimSuffix(pattern, "/**")
		if rel == pattern {
			return true
		}
		if strings.HasPrefix(rel, pattern+"/") {
			return true
		}
		if ok, err := doublestar.Match(pattern, rel); err == nil && ok {
			return true
		}
		// Match a bare name at any depth, so "node_modules" also excludes
		// "packages/app/node_modules".
		if !strings.Contains(pattern, "/") {
			for _, segment := range strings.Split(rel, "/") {
				if segment == pattern {
					return true
				}
			}
		}
	}
	return false
}

func copyRegularFile(src, dst string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	in, err := os.Open(src) //nolint:gosec // copying the user's own project tree
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	if perm == 0 {
		perm = 0o644
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm) //nolint:gosec // destination is inside the run directory
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// dirPerm makes sure a copied directory stays traversable by its owner even if
// the source had unusual permissions.
func dirPerm(p os.FileMode) os.FileMode {
	return p | 0o700
}

// restoreDependencyArtifacts lays down the artifacts produced by the jobs this
// one depends on, which is how output flows along `needs` edges.
func (s *Scheduler) restoreDependencyArtifacts(rs *runState, jobName, workspace string) error {
	node, ok := rs.graph.Nodes[jobName]
	if !ok {
		return fmt.Errorf("unknown job %q", jobName)
	}

	var restored int
	for _, need := range node.Needs {
		dep, ok := rs.jobs[need]
		if !ok || dep.record == nil {
			continue
		}
		files, err := s.db.ListJobArtifacts(rs.ctx, dep.record.ID)
		if err != nil {
			return fmt.Errorf("list artifacts of %q: %w", need, err)
		}
		for _, a := range files {
			if err := s.artifacts.Restore(a.StorePath, workspace, a.Path); err != nil {
				if errors.Is(err, artifacts.ErrPathEscape) {
					return fmt.Errorf("refusing to restore %q from %q: %w", a.Path, need, err)
				}
				return fmt.Errorf("restore %q from %q: %w", a.Path, need, err)
			}
			restored++
		}
	}
	if restored > 0 {
		s.logger.Debug("restored dependency artifacts",
			"run", rs.run.ID, "job", jobName, "files", restored)
	}
	return nil
}

// sanitizeDirName makes a job name usable as a directory name.
func sanitizeDirName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, name)
	if cleaned == "" {
		return "job"
	}
	if len(cleaned) > 64 {
		cleaned = cleaned[:64]
	}
	return cleaned
}
