package schedule

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// ErrMalformed matches every *MalformedError through errors.Is.
var ErrMalformed = errors.New("schedule: malformed")

// ErrViewRequired is returned when a floating schedule is expanded without a
// view zone to resolve its wall times in.
var ErrViewRequired = errors.New("schedule: floating schedule needs a view zone")

// Code names the class of a refusal. The values are the API vocabulary
// (details.code on a 400 validation_error), so they never change spelling.
type Code string

const (
	CodeSyntax        Code = "syntax"
	CodeUnknownPart   Code = "unknown_part"
	CodeDuplicatePart Code = "duplicate_part"
	CodeMissingPart   Code = "missing_part"
	CodeRange         Code = "range"
	CodeCombination   Code = "combination"
	CodeUnsupported   Code = "unsupported"
	CodeUnsatisfiable Code = "unsatisfiable"
	CodeZone          Code = "zone"
	CodeWall          Code = "wall"
)

// maxValueBytes bounds the offending text a MalformedError echoes back.
const maxValueBytes = 64

// MalformedError is the only error this package returns for bad input. It is
// built here and never wraps a third-party error, so no library text (some of
// it is wrong, TASK-4100) can reach a caller.
type MalformedError struct {
	Code   Code
	Part   string // "FREQ", "BYDAY", "UNTIL", "Zone", "RDATE", "" for the rule as a whole
	Value  string // offending text, truncated to 64 bytes
	Detail string // one short human sentence
}

func malformed(code Code, part, value, detail string) *MalformedError {
	if len(value) > maxValueBytes {
		value = value[:maxValueBytes]
		for value != "" && !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return &MalformedError{Code: code, Part: part, Value: value, Detail: detail}
}

// Error reads like "schedule: malformed rule: BYMONTHDAY=32: out of range
// 1..31 or -31..-1".
func (e *MalformedError) Error() string {
	kind := "rule"
	if e.Code == CodeZone || e.Code == CodeWall {
		kind = "spec"
	}
	var b strings.Builder
	b.WriteString("schedule: malformed " + kind)
	if e.Part != "" || e.Value != "" {
		b.WriteString(": ")
		b.WriteString(e.Part)
		if e.Value != "" {
			if e.Part != "" {
				b.WriteString("=")
			}
			b.WriteString(e.Value)
		}
	}
	if e.Detail != "" {
		b.WriteString(": " + e.Detail)
	}
	return b.String()
}

// Is makes errors.Is(err, ErrMalformed) true for any *MalformedError.
func (e *MalformedError) Is(target error) bool { return target == ErrMalformed }
