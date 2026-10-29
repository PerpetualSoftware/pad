package main

import (
	"bytes"
	pad "github.com/PerpetualSoftware/pad"
	"net/http"
	"strings"
	"testing"
)

// BUG-3999: any failed write that sent an op_key names it on stderr, so a
// retry can reuse it. A success prints nothing.
func TestBUG3999_FailedWritesPrintTheirKey(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, status int) (error, string)
	}{
		{"patch", func(t *testing.T, st int) (error, string) {
			bodies := patchServer(t, st, st)
			err := runPatch(t, "--old", "a", "--new", "b", "--op-key", "kk")
			_ = bodies
			return err, "kk"
		}},
		{"create", func(t *testing.T, st int) (error, string) {
			createServer(t, true, st, st)
			return runCreate(t, "--op-key", "kk"), "kk"
		}},
		{"comment", func(t *testing.T, st int) (error, string) {
			commentServer(t, true, st, st)
			return runComment(t, "--op-key", "kk"), "kk"
		}},
		{"note", func(t *testing.T, st int) (error, string) {
			appendServer(t, true, st, st)
			return runNote(t, "--op-key", "kk"), "kk"
		}},
	}
	for _, tc := range cases {
		for _, st := range []int{http.StatusServiceUnavailable, http.StatusConflict} {
			t.Run(tc.name+"/"+http.StatusText(st), func(t *testing.T) {
				err, key := tc.run(t, st)
				if err == nil {
					t.Fatal("the write must fail")
				}
				var buf bytes.Buffer
				writeRootStructuredError(&buf, err)
				want := "--op-key " + key
				if !strings.Contains(buf.String(), "idempotency key was "+key) || !strings.Contains(buf.String(), want) {
					t.Fatalf("stderr must name the key; got %q", buf.String())
				}
			})
		}
	}
}

func TestBUG3999_SuccessAndKeylessPrintNothing(t *testing.T) {
	patchServer(t)
	if err := runPatch(t, "--old", "a", "--new", "b"); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	writeRootStructuredError(&buf, nil)
	if buf.Len() != 0 {
		t.Fatalf("nil error printed %q", buf.String())
	}
}

func TestBUG3999_SkillNoLongerPromisesAutomaticRetrySafety(t *testing.T) {
	s := string(pad.PadSkill)
	if strings.Contains(s, "never applies it twice") {
		t.Fatal("SKILL.md still claims a re-run never applies a patch twice")
	}
	for _, want := range []string{"choose the key before the first attempt", "--op-key"} {
		if !strings.Contains(s, want) {
			t.Fatalf("SKILL.md must say %q", want)
		}
	}
}
