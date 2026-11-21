// Package secret holds a credential so it cannot be printed by accident.
//
// A Secret renders as Redacted through fmt (every verb), log/slog, errors,
// encoding/json and encoding.TextMarshaler. The value comes out only through
// Reveal, at the one place it is used: a request header, a hash, a driver.
//
// Because it marshals as Redacted, a Secret must not sit in a struct that is
// encoded for the wire, for a file or for CLI output: that struct would write
// the marker instead of the value. Keep such structs string-typed and Reveal
// into them at the boundary.
//
// The package imports the standard library only, so any package can use it.
package secret

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
)

// Redacted is what a Secret renders as, and the marker the log scrubber uses.
const Redacted = "REDACTED"

// Secret is a credential. The zero value is the empty secret.
//
// The value sits behind a pointer: fmt does not call Format on an unexported
// struct field but reflects into it, and a pointer shows only an address
// there. The [0]func() field makes Secret non-comparable, so == (which would
// compare pointers) does not compile; use Equal.
type Secret struct {
	_ [0]func()
	p *string
}

// New wraps s. An empty s gives the zero Secret.
func New(s string) Secret {
	if s == "" {
		return Secret{}
	}
	return Secret{p: &s}
}

// Reveal returns the credential. Call it only where the value is used.
func (s Secret) Reveal() string {
	if s.p == nil {
		return ""
	}
	return *s.p
}

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s.p == nil || *s.p == "" }

// Equal compares the secret with given in time that depends only on their
// lengths (subtle.ConstantTimeCompare): the length is not treated as secret.
func (s Secret) Equal(given string) bool {
	return subtle.ConstantTimeCompare([]byte(s.Reveal()), []byte(given)) == 1
}

// String returns Redacted.
func (s Secret) String() string { return Redacted }

// GoString returns a %#v form that carries no value.
func (s Secret) GoString() string { return `secret.Secret("` + Redacted + `")` }

// Format writes Redacted for every verb.
func (s Secret) Format(f fmt.State, verb rune) {
	if verb == 'v' && f.Flag('#') {
		_, _ = f.Write([]byte(s.GoString()))
		return
	}
	if verb == 'q' {
		_, _ = f.Write([]byte(`"` + Redacted + `"`))
		return
	}
	_, _ = f.Write([]byte(Redacted))
}

// LogValue makes slog print Redacted.
func (s Secret) LogValue() slog.Value { return slog.StringValue(Redacted) }

// MarshalJSON writes "REDACTED".
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }

// MarshalText writes REDACTED.
func (s Secret) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// UnmarshalJSON reads a JSON string. null leaves the value unchanged, as it
// does for a string field, and any other type gives the error a string field
// would.
func (s *Secret) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*s = New(v)
	return nil
}

// UnmarshalText reads the raw text, for decoders such as toml.
func (s *Secret) UnmarshalText(b []byte) error {
	*s = New(string(b))
	return nil
}
