package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Store holds collected artifacts on disk, outside any job workspace.
//
// The layout is <root>/<run id>/<job name>/<path as declared>, which keeps the
// store browsable by hand and makes it obvious which run produced what.
type Store struct {
	root string
	// MaxFileSize rejects individual files above this size. Zero disables the check.
	MaxFileSize int64
	// MaxTotalSize caps the bytes collected for a single job. Zero disables the check.
	MaxTotalSize int64
}

// NewStore prepares an artifact store rooted at dir.
func NewStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("artifacts: store directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("artifacts: resolve store dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("artifacts: create store dir: %w", err)
	}
	return &Store{root: abs}, nil
}

// Root is the store's base directory.
func (s *Store) Root() string { return s.root }

// File is one collected artifact.
type File struct {
	// Path is the artifact's location relative to the job workspace, i.e. the
	// name the pipeline author would recognise.
	Path string
	// StorePath is its location relative to the store root.
	StorePath string
	Size      int64
	SHA256    string
	Mode      os.FileMode
}

// CollectRequest describes what to collect from one job.
type CollectRequest struct {
	RunID   int64
	JobName string
	// Workspace is the absolute directory to collect from.
	Workspace string
	// Paths are globs relative to the workspace. `**` matches across directories
	// and a trailing `/` collects a directory recursively.
	Paths []string
	// Exclude removes matches from the selection.
	Exclude []string
}

// Collect copies every file matching the request into the store.
//
// Matching is done through an fs.FS rooted at the workspace, so a pattern cannot
// address anything outside it however it is written. Each match is then resolved
// through SafeResolve, which also rejects symlinks pointing out of the workspace.
func (s *Store) Collect(ctx context.Context, req CollectRequest) ([]File, error) {
	if req.Workspace == "" {
		return nil, errors.New("artifacts: workspace is required")
	}
	workspace, err := filepath.Abs(req.Workspace)
	if err != nil {
		return nil, fmt.Errorf("artifacts: resolve workspace: %w", err)
	}

	matches, err := s.match(workspace, req.Paths, req.Exclude)
	if err != nil {
		return nil, err
	}

	destBase, err := SafeJoin(s.root, filepath.Join(strconv.FormatInt(req.RunID, 10), sanitizeSegment(req.JobName)))
	if err != nil {
		return nil, err
	}

	var (
		out   []File
		total int64
	)
	for _, rel := range matches {
		if err := ctx.Err(); err != nil {
			return out, err
		}

		src, err := SafeResolve(workspace, rel)
		if err != nil {
			// A match that resolves outside the workspace is skipped rather than
			// failing the collection: it is almost always a stray symlink, and
			// the rest of the artifacts are still worth keeping.
			continue
		}
		info, err := os.Lstat(src)
		if err != nil {
			return nil, fmt.Errorf("artifacts: stat %s: %w", rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// Follow it only if it stays inside; SafeResolve already checked.
			info, err = os.Stat(src)
			if err != nil {
				continue // broken symlink
			}
		}
		if info.IsDir() {
			continue
		}
		if !info.Mode().IsRegular() {
			continue // devices, sockets and fifos are not artifacts
		}
		if s.MaxFileSize > 0 && info.Size() > s.MaxFileSize {
			return nil, fmt.Errorf("artifacts: %s is %d bytes, over the %d byte per-file limit",
				rel, info.Size(), s.MaxFileSize)
		}
		if s.MaxTotalSize > 0 && total+info.Size() > s.MaxTotalSize {
			return nil, fmt.Errorf("artifacts: collecting %s would exceed the %d byte per-job limit",
				rel, s.MaxTotalSize)
		}

		dest, err := SafeJoin(destBase, rel)
		if err != nil {
			return nil, err
		}
		sum, n, err := copyFile(src, dest, info.Mode().Perm())
		if err != nil {
			return nil, err
		}
		total += n

		storeRel, err := filepath.Rel(s.root, dest)
		if err != nil {
			return nil, fmt.Errorf("artifacts: relativise store path: %w", err)
		}
		out = append(out, File{
			Path:      filepath.ToSlash(rel),
			StorePath: filepath.ToSlash(storeRel),
			Size:      n,
			SHA256:    sum,
			Mode:      info.Mode().Perm(),
		})
	}
	return out, nil
}

// match expands the include patterns, removes the excludes, and returns a sorted,
// de-duplicated list of workspace-relative file paths.
func (s *Store) match(workspace string, include, exclude []string) ([]string, error) {
	fsys := os.DirFS(workspace)
	seen := make(map[string]bool)

	for _, raw := range include {
		pattern := NormalizePattern(raw)
		if pattern == "" {
			continue
		}
		if !doublestar.ValidatePattern(pattern) {
			return nil, fmt.Errorf("artifacts: %q is not a valid pattern", raw)
		}
		found, err := doublestar.Glob(fsys, pattern, doublestar.WithFilesOnly(), doublestar.WithNoFollow())
		if err != nil {
			return nil, fmt.Errorf("artifacts: match %q: %w", raw, err)
		}
		for _, m := range found {
			seen[m] = true
		}

		// A bare directory name should collect the directory, matching the
		// intuition that `paths: [dist]` and `paths: [dist/]` mean the same thing.
		if !strings.ContainsAny(pattern, "*?[{") {
			if info, err := fs.Stat(fsys, pattern); err == nil && info.IsDir() {
				sub, err := doublestar.Glob(fsys, pattern+"/**",
					doublestar.WithFilesOnly(), doublestar.WithNoFollow())
				if err != nil {
					return nil, fmt.Errorf("artifacts: match %q: %w", raw, err)
				}
				for _, m := range sub {
					seen[m] = true
				}
			}
		}
	}

	for _, raw := range exclude {
		pattern := NormalizePattern(raw)
		if pattern == "" {
			continue
		}
		for m := range seen {
			matched, err := doublestar.Match(pattern, m)
			if err != nil {
				return nil, fmt.Errorf("artifacts: exclude %q: %w", raw, err)
			}
			if matched {
				delete(seen, m)
			}
		}
	}

	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out, nil
}

// Open returns a reader for a stored artifact. storePath is validated against the
// store root, so a tampered database row cannot be used to read arbitrary files.
func (s *Store) Open(storePath string) (io.ReadCloser, error) {
	abs, err := SafeJoin(s.root, filepath.FromSlash(storePath))
	if err != nil {
		return nil, err
	}
	f, err := os.Open(abs) //nolint:gosec // path validated by SafeJoin
	if err != nil {
		return nil, fmt.Errorf("artifacts: open %s: %w", storePath, err)
	}
	return f, nil
}

// Stat returns file info for a stored artifact.
func (s *Store) Stat(storePath string) (os.FileInfo, error) {
	abs, err := SafeJoin(s.root, filepath.FromSlash(storePath))
	if err != nil {
		return nil, err
	}
	return os.Stat(abs)
}

// Restore copies a stored artifact into a destination workspace at rel.
//
// This is how artifacts flow along `needs`: before a job runs, the artifacts of
// everything it depends on are laid down in its workspace. Both ends of the copy
// are validated, so neither a hostile store path nor a hostile destination can
// escape.
func (s *Store) Restore(storePath, destWorkspace, rel string) error {
	src, err := SafeJoin(s.root, filepath.FromSlash(storePath))
	if err != nil {
		return err
	}
	dest, err := SafeResolve(destWorkspace, filepath.FromSlash(rel))
	if err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("artifacts: stat stored artifact: %w", err)
	}
	if _, _, err := copyFile(src, dest, info.Mode().Perm()); err != nil {
		return err
	}
	return nil
}

// Remove deletes a stored artifact and prunes directories left empty by it.
func (s *Store) Remove(storePath string) error {
	abs, err := SafeJoin(s.root, filepath.FromSlash(storePath))
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("artifacts: remove %s: %w", storePath, err)
	}
	s.pruneEmptyDirs(filepath.Dir(abs))
	return nil
}

// RemoveRun deletes every artifact of a run.
func (s *Store) RemoveRun(runID int64) error {
	dir, err := SafeJoin(s.root, strconv.FormatInt(runID, 10))
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("artifacts: remove run %d: %w", runID, err)
	}
	return nil
}

// pruneEmptyDirs walks up from dir removing empty directories, stopping at the
// store root so the root itself is never removed.
func (s *Store) pruneEmptyDirs(dir string) {
	for Contains(s.root, dir) && dir != s.root {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// copyFile copies src to dest, creating parents, and returns the SHA-256 of the
// bytes written along with the byte count. The hash is computed during the copy
// so the file is read exactly once.
func copyFile(src, dest string, mode os.FileMode) (string, int64, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return "", 0, fmt.Errorf("artifacts: create %s: %w", filepath.Dir(dest), err)
	}
	in, err := os.Open(src) //nolint:gosec // path validated by the caller
	if err != nil {
		return "", 0, fmt.Errorf("artifacts: open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	if mode == 0 {
		mode = 0o644
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) //nolint:gosec // path validated by the caller
	if err != nil {
		return "", 0, fmt.Errorf("artifacts: create %s: %w", dest, err)
	}

	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, hasher), in)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", 0, fmt.Errorf("artifacts: copy %s: %w", src, err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), n, nil
}

// sanitizeSegment makes a job name safe to use as a single path segment.
//
// SafeJoin already guarantees containment, so this is about producing a name that
// is unambiguous to read: any run of dots is collapsed so a sanitised name can
// never contain a ".." that invites a second look.
func sanitizeSegment(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, name)
	for strings.Contains(cleaned, "..") {
		cleaned = strings.ReplaceAll(cleaned, "..", "-")
	}
	cleaned = strings.Trim(cleaned, ".")
	if cleaned == "" {
		return "job"
	}
	return cleaned
}

// DownloadName suggests a filename for a single-artifact download.
func DownloadName(artifactPath string) string {
	base := path.Base(filepath.ToSlash(artifactPath))
	if base == "." || base == "/" || base == "" {
		return "artifact"
	}
	return base
}
