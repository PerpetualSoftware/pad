// Package jsonscan reads a JSON document one token at a time, for the checks
// that have to see every token (BUG-2812): the NUL gate on a request body, the
// store guard's walk of a JSON column, and the repeated-member refusal.
//
// WHY A THIRD-PARTY MODULE. encoding/json's json.Decoder.Token boxes every
// token in an interface and doubles its buffer until it holds the whole input.
// On a 1.54 MiB request body it measured 2.7x slower and 2.3x larger than
// decoding the same body into a map[string]any tree, which is the cost the
// token walk was meant to remove (BUG-2812 checkpoint 1). jsontext reads the
// same body with a fraction of either.
//
// THIS FILE IS THE ONLY IMPORTER of github.com/go-json-experiment/json, by
// ruling (BUG-2812, day 80). The module is the Go team's staging copy of
// encoding/json/v2. THE EXIT: when encoding/json/jsontext ships without
// GOEXPERIMENT, replace the import below with "encoding/json/jsontext", delete
// the module from go.mod, and update nix/package.nix's vendorHash. Nothing
// outside this file names jsontext, so that is the whole change.
package jsonscan

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode"
	"unicode/utf8"

	"github.com/go-json-experiment/json/jsontext"
)

// Kind is a token's kind: the first byte of its JSON spelling.
type Kind byte

const (
	ObjectStart Kind = '{'
	ObjectEnd   Kind = '}'
	ArrayStart  Kind = '['
	ArrayEnd    Kind = ']'
	String      Kind = '"'
	// Scalar is a number, true, false or null, none of which a check here
	// reads. A number is never converted, so a literal no float64 holds
	// (1e999) is an ordinary token rather than a failed decode.
	Scalar Kind = '0'
)

// ErrMalformed reports input that is not exactly one well-formed JSON value.
var ErrMalformed = errors.New("jsonscan: malformed JSON")

// Scanner reads one JSON value's tokens.
//
// It accepts what encoding/json v1's Unmarshal accepts, so that a check built
// on it answers about the same documents the typed decode reads: duplicate
// object names are allowed (v1 keeps them; the caller decides), invalid UTF-8
// inside strings is allowed (v1 replaces it), and anything after the value
// other than whitespace is refused, as Unmarshal refuses it.
type Scanner struct {
	dec  *jsontext.Decoder
	buf  []byte
	done bool
	// Raw, when set, makes Next return a String's text UNDECODED (escapes as
	// written), skipping the unquote. For a caller that only needs to know a
	// string is there: decoding a long value it will not read is most of the
	// cost of scanning it.
	Raw bool
}

// NewScanner scans doc. doc must not change while the Scanner is in use.
func NewScanner(doc []byte) *Scanner {
	// A *bytes.Buffer is read in place by jsontext, without the copy an
	// io.Reader forces.
	return &Scanner{dec: jsontext.NewDecoder(bytes.NewBuffer(doc),
		jsontext.AllowDuplicateNames(true),
		jsontext.AllowInvalidUTF8(true),
	)}
}

// Next returns the next token. For a String, which is either a value or an
// object member's name, str is its DECODED text (unless Raw is set) and is
// valid only until the next call. For every other kind str is nil.
//
// After the value's last token, Next returns io.EOF when nothing but
// whitespace follows, and ErrMalformed otherwise. Any syntax error is
// ErrMalformed.
func (s *Scanner) Next() (k Kind, str []byte, err error) {
	if s.done {
		// The value is complete. Anything but the end of input is a second
		// value, which Unmarshal refuses.
		if _, err := s.dec.ReadToken(); err == io.EOF {
			return 0, nil, io.EOF
		}
		return 0, nil, ErrMalformed
	}
	switch s.dec.PeekKind() {
	case '"':
		raw, err := s.dec.ReadValue()
		if err != nil {
			return 0, nil, ErrMalformed
		}
		body := raw[1 : len(raw)-1]
		if !s.Raw && bytes.IndexByte(body, '\\') >= 0 {
			// Only an escape can make the decoded text differ from the raw
			// bytes, so an escape-free string is returned without a copy.
			s.buf, err = jsontext.AppendUnquote(s.buf[:0], raw)
			if err != nil {
				// jsontext refuses an escape v1 accepts: a lone surrogate
				// (`\ud800`), which v1 decodes to U+FFFD. Calling that
				// malformed would make every check report NOTHING for the
				// whole body while the typed decode reads it, so any NUL
				// beside it would pass. v1's own unquote is the answer.
				var v string
				if json.Unmarshal(raw, &v) != nil {
					return 0, nil, ErrMalformed
				}
				s.buf = append(s.buf[:0], v...)
			}
			body = s.buf
		}
		s.markIfComplete()
		return String, body, nil
	case '{', '}', '[', ']':
		tok, err := s.dec.ReadToken()
		if err != nil {
			return 0, nil, ErrMalformed
		}
		s.markIfComplete()
		return Kind(tok.Kind()), nil, nil
	case 'n', 'f', 't', '0':
		if _, err := s.dec.ReadValue(); err != nil {
			return 0, nil, ErrMalformed
		}
		s.markIfComplete()
		return Scalar, nil, nil
	default:
		// PeekKind answers 0 at the end of input or on a syntax error.
		// Before the value is complete, both are malformed input.
		return 0, nil, ErrMalformed
	}
}

func (s *Scanner) markIfComplete() {
	if s.dec.StackDepth() == 0 {
		s.done = true
	}
}

// FoldName folds an object member name the way encoding/json v1 does when it
// matches a key to a struct field: ASCII letters upper-cased, and every other
// rune replaced by the smallest rune in its simple case-folding orbit. Two
// names that fold equal land in the same struct field.
func FoldName(name []byte) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); {
		c := name[i]
		if c < utf8.RuneSelf {
			if 'a' <= c && c <= 'z' {
				c -= 'a' - 'A'
			}
			out = append(out, c)
			i++
			continue
		}
		r, n := utf8.DecodeRune(name[i:])
		for {
			r2 := unicode.SimpleFold(r)
			if r2 <= r {
				r = r2
				break
			}
			r = r2
		}
		out = utf8.AppendRune(out, r)
		i += n
	}
	return string(out)
}
