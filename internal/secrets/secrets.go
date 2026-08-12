// Package secrets sources sensitive values for job environments and exposes the
// set of literals that must be masked from captured output.
//
// Secrets are deliberately never written to SQLite. They live in the process for
// the lifetime of a run, are injected into job environments, and every value is
// registered for redaction so it is masked before log bytes reach disk.
package secrets

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
)

// MinRedactLength is the shortest secret value that is masked. Redacting very
// short values would scribble over unrelated text ("a", "1") and make logs useless.
const MinRedactLength = 4

// Store holds the resolved secrets for a project.
type Store struct {
	mu     sync.RWMutex
	values map[string]string
}

// New returns an empty store.
func New() *Store { return &Store{values: make(map[string]string)} }

// Set adds or replaces a secret.
func (s *Store) Set(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
}

// Get returns a secret and whether it was present.
func (s *Store) Get(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[key]
	return v, ok
}

// Len reports how many secrets are loaded.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.values)
}

// Names returns the secret keys in sorted order. Only names are exposed: this is
// what the API and `forge settings` display, never the values.
func (s *Store) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.values))
	for k := range s.values {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Env returns the secrets as KEY=VALUE pairs for injection into a job environment.
func (s *Store) Env() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.values))
	for k, v := range s.values {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Values returns the literals to mask in logs, longest first so that a secret
// containing another secret is replaced before its substring is.
func (s *Store) Values() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.values))
	for _, v := range s.values {
		if len(v) >= MinRedactLength {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// LoadFile reads a KEY=VALUE file into the store. A missing file is not an error:
// secrets are optional. The file's permissions are checked because a world-readable
// secrets file is a real problem worth surfacing.
func (s *Store) LoadFile(path string) error {
	f, err := os.Open(path) //nolint:gosec // operator-supplied path by design
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open secrets file: %w", err)
	}
	defer func() { _ = f.Close() }()

	if st, err := f.Stat(); err == nil && st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("secrets file %s is group/world readable (mode %#o); run: chmod 600 %s",
			path, st.Mode().Perm(), path)
	}

	pairs, err := Parse(f)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range pairs {
		s.values[k] = v
	}
	return nil
}

// LoadEnv copies the named host environment variables into the store. Variables
// that are not set are ignored rather than being treated as empty secrets.
func (s *Store) LoadEnv(names []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range names {
		if v, ok := os.LookupEnv(name); ok {
			s.values[name] = v
		}
	}
}

// Parse reads KEY=VALUE lines. Blank lines and `#` comments are skipped, an
// optional `export ` prefix is tolerated, and single or double quoted values are
// unquoted. This is the common ".env" dialect, kept small on purpose.
func Parse(r io.Reader) (map[string]string, error) {
	out := make(map[string]string)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		key, value, found := strings.Cut(text, "=")
		if !found {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE, got %q", line, text)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("line %d: empty key", line)
		}
		if !validKey(key) {
			return nil, fmt.Errorf("line %d: invalid key %q", line, key)
		}
		out[key] = unquote(strings.TrimSpace(value))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// unquote strips one matching pair of surrounding quotes. Only double-quoted
// values get escape processing, matching shell semantics closely enough.
func unquote(v string) string {
	if len(v) >= 2 {
		if v[0] == '"' && v[len(v)-1] == '"' {
			inner := v[1 : len(v)-1]
			inner = strings.ReplaceAll(inner, `\n`, "\n")
			inner = strings.ReplaceAll(inner, `\t`, "\t")
			inner = strings.ReplaceAll(inner, `\"`, `"`)
			return inner
		}
		if v[0] == '\'' && v[len(v)-1] == '\'' {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// validKey reports whether name is a usable environment variable name.
func validKey(name string) bool {
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return name != ""
}
