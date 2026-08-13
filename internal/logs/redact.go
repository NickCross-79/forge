package logs

import (
	"bytes"
	"sort"
)

// Mask replaces a secret value wherever it appears in captured output.
const Mask = "***"

// Redactor removes known secret literals from log output. Redaction happens on
// the write path, before bytes reach disk, so a secret is never persisted even
// briefly and cannot be recovered by reading the log file directly.
type Redactor struct {
	// values is sorted longest-first so that a secret which contains a shorter
	// secret is masked as a whole rather than being partially rewritten.
	values [][]byte
	maxLen int
}

// NewRedactor builds a redactor for the given literals. Values shorter than
// MinLength are ignored: masking them would corrupt unrelated output.
func NewRedactor(values []string) *Redactor {
	r := &Redactor{}
	seen := make(map[string]bool, len(values))
	for _, v := range values {
		if len(v) < MinLength || seen[v] {
			continue
		}
		seen[v] = true
		r.values = append(r.values, []byte(v))
	}
	sort.Slice(r.values, func(i, j int) bool {
		if len(r.values[i]) != len(r.values[j]) {
			return len(r.values[i]) > len(r.values[j])
		}
		return bytes.Compare(r.values[i], r.values[j]) < 0
	})
	for _, v := range r.values {
		if len(v) > r.maxLen {
			r.maxLen = len(v)
		}
	}
	return r
}

// MinLength is the shortest literal that will be masked.
const MinLength = 4

// Enabled reports whether there is anything to redact.
func (r *Redactor) Enabled() bool { return r != nil && len(r.values) > 0 }

// MaxSecretLen is the length of the longest tracked secret. Writers use it to
// decide how much trailing data to hold back when flushing an unterminated line,
// so a secret split across two writes is still caught.
func (r *Redactor) MaxSecretLen() int {
	if r == nil {
		return 0
	}
	return r.maxLen
}

// Redact returns b with every known secret replaced by Mask. The input is not
// modified; when nothing matches the original slice is returned unchanged.
func (r *Redactor) Redact(b []byte) []byte {
	if !r.Enabled() || len(b) == 0 {
		return b
	}
	out := b
	copied := false
	for _, secret := range r.values {
		if !bytes.Contains(out, secret) {
			continue
		}
		if !copied {
			out = append([]byte(nil), out...)
			copied = true
		}
		out = bytes.ReplaceAll(out, secret, []byte(Mask))
	}
	return out
}

// RedactString is the string form of Redact, used for error messages and job
// metadata that are persisted alongside logs.
func (r *Redactor) RedactString(s string) string {
	if !r.Enabled() || s == "" {
		return s
	}
	return string(r.Redact([]byte(s)))
}
