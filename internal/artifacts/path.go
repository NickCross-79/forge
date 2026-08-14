// Package artifacts collects files from a job workspace, stores them outside it,
// and serves them back for download or for seeding a dependent job's workspace.
//
// Every path that crosses this package's boundary is untrusted: patterns come
// from a pipeline file and matched names come from the filesystem, where a job's
// own commands may have created symlinks. All of it is funnelled through SafeJoin
// so that nothing can read or write outside the directory it is confined to.
package artifacts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrPathEscape is returned when a path would resolve outside its base directory.
var ErrPathEscape = errors.New("path escapes its base directory")

// SafeJoin resolves rel against base and guarantees the result stays inside base.
//
// It rejects absolute paths and any path that climbs out with "..", then verifies
// containment on the cleaned result. This is the lexical half of the defence; use
// SafeResolve when the path may exist and could be a symlink.
func SafeJoin(base, rel string) (string, error) {
	if base == "" {
		return "", errors.New("artifacts: base directory is required")
	}
	if rel == "" {
		return filepath.Clean(base), nil
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return "", fmt.Errorf("artifacts: %q is absolute: %w", rel, ErrPathEscape)
	}
	// Reject Windows drive-relative paths such as `C:foo`, which are neither
	// absolute nor safely relative.
	if len(rel) >= 2 && rel[1] == ':' {
		return "", fmt.Errorf("artifacts: %q is drive-relative: %w", rel, ErrPathEscape)
	}

	cleanBase := filepath.Clean(base)
	joined := filepath.Clean(filepath.Join(cleanBase, rel))
	if !Contains(cleanBase, joined) {
		return "", fmt.Errorf("artifacts: %q: %w", rel, ErrPathEscape)
	}
	return joined, nil
}

// SafeResolve is SafeJoin plus a symlink check.
//
// A job can create a symlink inside its own workspace pointing anywhere on the
// host; collecting through it would copy files the pipeline never selected. The
// resolved target must therefore also live inside base. Paths that do not exist
// yet are checked against their nearest existing ancestor, which is what makes
// this usable for destinations as well as sources.
func SafeResolve(base, rel string) (string, error) {
	joined, err := SafeJoin(base, rel)
	if err != nil {
		return "", err
	}

	realBase, err := filepath.EvalSymlinks(filepath.Clean(base))
	if err != nil {
		return "", fmt.Errorf("artifacts: resolve base %s: %w", base, err)
	}

	// Walk up to the nearest existing ancestor so a not-yet-created file can
	// still be validated.
	probe := joined
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			if !Contains(realBase, resolved) {
				return "", fmt.Errorf("artifacts: %q resolves outside the workspace: %w", rel, ErrPathEscape)
			}
			break
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("artifacts: resolve %q: %w", rel, err)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	return joined, nil
}

// Contains reports whether target is base or lives beneath it. Both arguments
// must be cleaned absolute paths.
func Contains(base, target string) bool {
	if base == target {
		return true
	}
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// NormalizePattern converts a user-supplied glob to the forward-slash form the
// matcher expects, and expands the "collect this whole directory" shorthand of a
// trailing slash into a recursive pattern.
func NormalizePattern(p string) string {
	p = strings.TrimSpace(p)
	p = filepath.ToSlash(p)
	p = strings.TrimPrefix(p, "./")
	if p == "" {
		return ""
	}
	if strings.HasSuffix(p, "/") {
		return strings.TrimSuffix(p, "/") + "/**"
	}
	return p
}
