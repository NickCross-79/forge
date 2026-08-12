package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	input := `
# a comment
KEY=value
export EXPORTED=exported-value

QUOTED="double quoted"
SINGLE='single quoted'
EMPTY=
SPACES = padded
WITH_EQUALS=a=b=c
ESCAPES="line\nbreak\ttab\"quote"
URL=https://example.com/path?a=1#frag
`
	got, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	want := map[string]string{
		"KEY":         "value",
		"EXPORTED":    "exported-value",
		"QUOTED":      "double quoted",
		"SINGLE":      "single quoted",
		"EMPTY":       "",
		"SPACES":      "padded",
		"WITH_EQUALS": "a=b=c",
		"ESCAPES":     "line\nbreak\ttab\"quote",
		// A `#` inside a value is part of the value, not a comment.
		"URL": "https://example.com/path?a=1#frag",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("parsed %d keys, want %d: %v", len(got), len(want), got)
	}
}

func TestParseRejectsMalformedInput(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"no equals", "JUST_A_KEY\n", "expected KEY=VALUE"},
		{"empty key", "=value\n", "empty key"},
		{"invalid key", "BAD-KEY=value\n", "invalid key"},
		{"leading digit", "1KEY=value\n", "invalid key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.input))
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want an error", tc.input)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestStoreSetGetNames(t *testing.T) {
	s := New()
	if s.Len() != 0 {
		t.Errorf("a new store has %d entries, want 0", s.Len())
	}

	s.Set("BETA", "2")
	s.Set("ALPHA", "1")

	if v, ok := s.Get("ALPHA"); !ok || v != "1" {
		t.Errorf("Get(ALPHA) = %q, %v", v, ok)
	}
	if _, ok := s.Get("MISSING"); ok {
		t.Error("Get(MISSING) reported present")
	}
	// Names are sorted so every listing is stable.
	names := s.Names()
	if len(names) != 2 || names[0] != "ALPHA" || names[1] != "BETA" {
		t.Errorf("Names() = %v, want sorted [ALPHA BETA]", names)
	}
	env := s.Env()
	if len(env) != 2 || env[0] != "ALPHA=1" || env[1] != "BETA=2" {
		t.Errorf("Env() = %v, want sorted KEY=VALUE pairs", env)
	}
}

func TestStoreValuesForRedaction(t *testing.T) {
	s := New()
	s.Set("SHORT", "ab") // below MinRedactLength
	s.Set("A", "example-value")
	s.Set("B", "a-much-longer-example-value")

	values := s.Values()
	if len(values) != 2 {
		t.Fatalf("Values() = %v, want the two long-enough values", values)
	}
	// Longest first, so a secret containing another is masked whole.
	if len(values[0]) < len(values[1]) {
		t.Errorf("Values() = %v, want longest first", values)
	}
	for _, v := range values {
		if v == "ab" {
			t.Error("a value below the minimum length was offered for redaction")
		}
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.env")
	if err := os.WriteFile(path, []byte("TOKEN=example-value\nOTHER=example-other\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := New()
	if err := s.LoadFile(path); err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if v, _ := s.Get("TOKEN"); v != "example-value" {
		t.Errorf("TOKEN = %q", v)
	}
}

func TestLoadFileMissingIsNotAnError(t *testing.T) {
	// Secrets are optional; a project without them must still run.
	s := New()
	if err := s.LoadFile(filepath.Join(t.TempDir(), "absent.env")); err != nil {
		t.Errorf("LoadFile(missing) error = %v, want nil", err)
	}
	if s.Len() != 0 {
		t.Error("a missing file should load nothing")
	}
}

func TestLoadFileRejectsLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.env")
	if err := os.WriteFile(path, []byte("TOKEN=example-value\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New()
	err := s.LoadFile(path)
	if err == nil {
		t.Fatal("a world-readable secrets file was accepted")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error = %q, should tell the operator how to fix it", err)
	}
	if s.Len() != 0 {
		t.Error("secrets were loaded from a file with unsafe permissions")
	}
}

func TestLoadEnv(t *testing.T) {
	t.Setenv("FORGE_TEST_PRESENT", "from-host")

	s := New()
	s.LoadEnv([]string{"FORGE_TEST_PRESENT", "FORGE_TEST_ABSENT"})

	if v, ok := s.Get("FORGE_TEST_PRESENT"); !ok || v != "from-host" {
		t.Errorf("FORGE_TEST_PRESENT = %q, %v", v, ok)
	}
	// An unset variable is skipped rather than stored as an empty secret, which
	// would otherwise mask every empty string in the logs.
	if _, ok := s.Get("FORGE_TEST_ABSENT"); ok {
		t.Error("an unset variable was recorded as a secret")
	}
}

func TestValidKey(t *testing.T) {
	for _, k := range []string{"A", "_", "KEY", "key_1", "_private", "A1"} {
		if !validKey(k) {
			t.Errorf("validKey(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"", "1KEY", "KEY-1", "KEY.1", "KEY 1", "KÉY"} {
		if validKey(k) {
			t.Errorf("validKey(%q) = true, want false", k)
		}
	}
}
