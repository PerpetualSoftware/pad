package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// IDEA-2937: `pad item show <ref> --format markdown` is the machine-facing read
// of an item body, and `pad item update <ref> --stdin` is the write. Together
// they are the read-modify-write shape every agent and tool reaches for, so the
// pair has to be a FIXED POINT: reading a body and writing those same bytes back
// must leave the stored body byte-identical.
//
// It was not. `show` printed the body with fmt.Println, adding a newline the
// stored body did not have, and the write path stores what it is sent verbatim
// (measured against a live server: a body with no trailing newline, and one with
// three, both stored exactly as sent). So each round trip grew the body by one
// byte, without bound — 518, 519, 520, 521, 522 on the reporter's fixture — and
// a `$(...)` capture LOST a byte instead, because command substitution strips
// every trailing newline from what it captures.
//
// Both directions were the same missing byte-fidelity on the read side, which is
// why these tests assert EQUALITY with the body rather than `strings.Contains`.

func markdownShowBodies() map[string]string {
	return map[string]string{
		// The common case: a body that already ends with a newline. This is the
		// one that grew unboundedly, because Println always added a second one.
		"trailing newline": "line one\nline two\n",
		// A body with NO trailing newline. This is what a `$(...)`-based tool
		// writes back, so it is a real stored shape, not a synthetic one — and
		// it is the case a "add a newline only when one is missing" fix would
		// still get wrong.
		"no trailing newline": "line one\nline two",
		// Deliberate blank lines at the end of a body are content. Nothing on
		// this path may normalize them away.
		"several trailing newlines": "line one\n\n\n",
		// A body that is only whitespace still round-trips as itself.
		"newline only": "\n",
		// An empty body emits nothing at all — not a bare newline.
		"empty": "",
	}
}

// The bytes `show --format markdown` writes to stdout must BE the stored body:
// no prefix, no suffix, nothing added or removed.
func TestItemShowMarkdown_EmitsBodyVerbatim(t *testing.T) {
	for name, body := range markdownShowBodies() {
		t.Run(name, func(t *testing.T) {
			setupFormatRoutingTest(t, jsonHandler(t, map[string]any{
				"/items/DOC-1": models.Item{Slug: "doc-1", Title: "a doc", Content: body},
			}))
			formatFlag = "markdown"

			cmd := showCmd()
			cmd.SetArgs([]string{"DOC-1"})

			var execErr error
			out := captureStdout(t, func() { execErr = cmd.Execute() })
			if execErr != nil {
				t.Fatalf("execute item show --format markdown: %v\noutput:\n%q", execErr, out)
			}

			if out != body {
				t.Errorf("markdown output is not the stored body verbatim\n stored: %q\n emitted: %q\n delta: %+d bytes",
					body, out, len(out)-len(body))
			}
		})
	}
}

// The write half: `--stdin` must send exactly the bytes it was given. Paired
// with the test above, this is the fixed point — what `show` emits is what
// `update` sends, so a body survives any number of round trips unchanged.
func TestItemUpdateStdin_SendsBodyVerbatim(t *testing.T) {
	for name, body := range markdownShowBodies() {
		t.Run(name, func(t *testing.T) {
			var sent string
			var seenPatch bool

			setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch {
					seenPatch = true
					var payload struct {
						Content *string `json:"content"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Errorf("decode PATCH body: %v", err)
					} else if payload.Content == nil {
						t.Errorf("PATCH carried no content key")
					} else {
						sent = *payload.Content
					}
				}
				_ = json.NewEncoder(w).Encode(models.Item{Slug: "doc-1", Title: "a doc", Content: body})
			}))
			formatFlag = "table"

			restoreStdin := feedStdin(t, body)
			defer restoreStdin()

			cmd := updateCmd()
			cmd.SetArgs([]string{"DOC-1", "--stdin"})

			var execErr error
			out := captureStdout(t, func() { execErr = cmd.Execute() })
			if execErr != nil {
				t.Fatalf("execute item update --stdin: %v\noutput:\n%s", execErr, out)
			}
			if !seenPatch {
				t.Fatalf("no PATCH reached the server; output:\n%s", out)
			}
			if sent != body {
				t.Errorf("--stdin did not send the body verbatim\n given: %q\n sent: %q\n delta: %+d bytes",
					body, sent, len(sent)-len(body))
			}
		})
	}
}

// feedStdin points os.Stdin at a pipe carrying s, and returns a restore func.
// The update command reads os.Stdin directly (io.ReadAll), not cmd.InOrStdin,
// so cobra's SetIn is not enough here — the same reason captureStdout swaps
// os.Stdout rather than using SetOut.
func feedStdin(t *testing.T, s string) func() {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdin
	os.Stdin = r

	go func() {
		_, _ = io.Copy(w, strings.NewReader(s))
		_ = w.Close()
	}()

	return func() { os.Stdin = orig }
}
