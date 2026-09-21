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

// BUG-3100 (public mirror: GitHub #1391). `--stdin` with a blank body used to
// succeed: `item create` minted a content-less item and `item update` REPLACED
// the body with nothing, both reporting success. A lost heredoc arrives exactly
// like that, so an agent harness that dropped stdin produced hollow or wiped
// items with no signal. Blank — empty OR whitespace-only (lead ruling: a lone
// newline is the likeliest shape of a lost heredoc) — is now refused on both
// doors, before any request. Clearing a body on purpose moves to its own flag,
// `item update --clear-content`.

// withStdin replaces os.Stdin with a pipe carrying body for the duration of
// the test.
func withStdin(t *testing.T, body string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	_ = w.Close()
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		_ = r.Close()
	})
}

// recordingServer answers every request with a stub item and records each
// request's method, path and decoded JSON body.
type recordedRequest struct {
	method string
	path   string
	body   map[string]any
}

func recordingServer(t *testing.T) *[]recordedRequest {
	t.Helper()
	var reqs []recordedRequest
	setupFormatRoutingTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recordedRequest{method: r.Method, path: r.URL.Path}
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &rec.body)
			}
		}
		reqs = append(reqs, rec)
		_ = json.NewEncoder(w).Encode(models.Item{Slug: "task-5", CollectionSlug: "tasks", Title: "t"})
	}))
	formatFlag = "table"
	return &reqs
}

func writesOf(reqs []recordedRequest, method string) []recordedRequest {
	var out []recordedRequest
	for _, r := range reqs {
		if r.method == method {
			out = append(out, r)
		}
	}
	return out
}

var blankBodies = map[string]string{
	"empty":           "",
	"lone newline":    "\n",
	"whitespace only": "  \t\n\n ",
}

func TestItemCreateStdin_BlankRefused(t *testing.T) {
	for name, body := range blankBodies {
		t.Run(name, func(t *testing.T) {
			reqs := recordingServer(t)
			withStdin(t, body)
			cmd := createCmd()
			cmd.SetArgs([]string{"tasks", "a title", "--stdin"})

			var execErr error
			out := captureStdout(t, func() { execErr = cmd.Execute() })
			if execErr == nil {
				t.Fatalf("a blank --stdin body must be refused; got success:\n%s", out)
			}
			if !strings.Contains(execErr.Error(), "--stdin") || !strings.Contains(execErr.Error(), "nothing was sent") {
				t.Errorf("the refusal must name --stdin and say nothing was sent; got %q", execErr)
			}
			// ZERO requests, not "no POST": a refusal after a schema fetch would
			// still be a refusal, but the rule is that nothing happens.
			if len(*reqs) != 0 {
				t.Errorf("a refused create must not reach the server; it made %d request(s)", len(*reqs))
			}
			if strings.TrimSpace(out) != "" {
				t.Errorf("stdout must stay empty on a refusal (it is parseable output); got %q", out)
			}
		})
	}
}

func TestItemCreateStdin_BodyStillCreates(t *testing.T) {
	reqs := recordingServer(t)
	withStdin(t, "# Real body\n\nwith text\n")
	cmd := createCmd()
	cmd.SetArgs([]string{"tasks", "a title", "--stdin"})

	var execErr error
	out := captureStdout(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("a non-blank --stdin must still create: %v\n%s", execErr, out)
	}
	posts := writesOf(*reqs, http.MethodPost)
	if len(posts) != 1 {
		t.Fatalf("expected one POST; got %+v", *reqs)
	}
	if got := posts[0].body["content"]; got != "# Real body\n\nwith text\n" {
		t.Errorf("content sent = %q; want the stdin body verbatim", got)
	}
}

func TestItemUpdateStdin_BlankRefused(t *testing.T) {
	for name, body := range blankBodies {
		t.Run(name, func(t *testing.T) {
			reqs := recordingServer(t)
			withStdin(t, body)
			cmd := updateCmd()
			cmd.SetArgs([]string{"TASK-5", "--stdin"})

			var execErr error
			out := captureStdout(t, func() { execErr = cmd.Execute() })
			if execErr == nil {
				t.Fatalf("a blank --stdin body must be refused on update; got success:\n%s", out)
			}
			// The refusal must name the door that does what the caller may
			// have meant, or it removes the only way to clear a body.
			if !strings.Contains(execErr.Error(), "--clear-content") {
				t.Errorf("the update refusal must point at --clear-content; got %q", execErr)
			}
			if len(*reqs) != 0 {
				t.Errorf("a refused update must not reach the server; it made %d request(s)", len(*reqs))
			}
		})
	}
}

func TestItemUpdateStdin_BodyStillReplaces(t *testing.T) {
	reqs := recordingServer(t)
	withStdin(t, "new body\n")
	cmd := updateCmd()
	cmd.SetArgs([]string{"TASK-5", "--stdin"})

	var execErr error
	out := captureStdout(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("a non-blank --stdin update must still work: %v\n%s", execErr, out)
	}
	patches := writesOf(*reqs, http.MethodPatch)
	if len(patches) != 1 || patches[0].body["content"] != "new body\n" {
		t.Fatalf("expected one PATCH carrying the stdin body; got %+v", *reqs)
	}
}

func TestItemUpdate_ClearContent(t *testing.T) {
	t.Run("sends an explicit empty body and says so", func(t *testing.T) {
		reqs := recordingServer(t)
		cmd := updateCmd()
		cmd.SetArgs([]string{"TASK-5", "--clear-content"})

		var execErr error
		out := captureStdout(t, func() { execErr = cmd.Execute() })
		if execErr != nil {
			t.Fatalf("--clear-content: %v\n%s", execErr, out)
		}
		patches := writesOf(*reqs, http.MethodPatch)
		if len(patches) != 1 {
			t.Fatalf("expected one PATCH; got %+v", *reqs)
		}
		got, present := patches[0].body["content"]
		if !present || got != "" {
			t.Errorf("--clear-content must send content present-and-empty; got present=%v value=%q", present, got)
		}
		if !strings.Contains(out, "body cleared") {
			t.Errorf("the success output must say the body was cleared; got %q", out)
		}
	})

	for name, extra := range map[string][]string{
		"with --stdin":   {"--stdin"},
		"with --content": {"--content", "x"},
	} {
		t.Run("refused "+name, func(t *testing.T) {
			reqs := recordingServer(t)
			withStdin(t, "a body\n")
			cmd := updateCmd()
			cmd.SetArgs(append([]string{"TASK-5", "--clear-content"}, extra...))

			var execErr error
			_ = captureStdout(t, func() { execErr = cmd.Execute() })
			if execErr == nil || !strings.Contains(execErr.Error(), "--clear-content conflicts") {
				t.Fatalf("--clear-content %s must be refused as a conflict; got %v", name, execErr)
			}
			if len(*reqs) != 0 {
				t.Errorf("a refused conflict must not reach the server; it made %d request(s)", len(*reqs))
			}
		})
	}
}

// `--content ""` stays a no-op (lead ruling 4): making it a clear would turn an
// unset shell variable into a wipe. Pinned so that is a decision, not a gap.
func TestItemUpdate_EmptyContentFlagStillNoOp(t *testing.T) {
	reqs := recordingServer(t)
	cmd := updateCmd()
	cmd.SetArgs([]string{"TASK-5", "--content", "", "--status", "done"})

	var execErr error
	out := captureStdout(t, func() { execErr = cmd.Execute() })
	if execErr != nil {
		t.Fatalf("--content \"\" with another change must still succeed: %v\n%s", execErr, out)
	}
	for _, p := range writesOf(*reqs, http.MethodPatch) {
		if _, present := p.body["content"]; present {
			t.Errorf(`--content "" must not put a content key on the wire; PATCH body = %+v`, p.body)
		}
	}
}

// The rest of the stdin population (lead ruling 5), each pinned by evidence
// rather than by reading. Population: `os.Stdin` in non-test cmd/pad files —
// item create/update (above), item import (server-side; pinned in
// internal/server TestImportArtifactBlankRejected), note/decide, workspace
// context, collection --schema, and the interactive auth/configure/init
// prompts, which read answers rather than bodies and are out of scope.

// note/decide: details are OPTIONAL, so a blank stdin is a legitimate "no
// details" and stays accepted. What it must not do is error.
func TestStructuredEntryStdin_BlankIsNoDetails(t *testing.T) {
	for name, body := range blankBodies {
		t.Run(name, func(t *testing.T) {
			withStdin(t, body)
			got, err := readStructuredEntryBody()
			if err != nil {
				t.Fatalf("a blank details body must be accepted as no details: %v", err)
			}
			if got != "" {
				t.Errorf("blank details must read as empty; got %q", got)
			}
		})
	}
}

// workspace context --stdin: a blank body is not JSON, so it is already refused.
func TestWorkspaceContextStdin_BlankRefused(t *testing.T) {
	for name, body := range blankBodies {
		t.Run(name, func(t *testing.T) {
			withStdin(t, body)
			if _, err := readWorkspaceContextInput("", true); err == nil {
				t.Fatal("a blank workspace-context body must be refused")
			}
		})
	}
}

// collection create/update --schema -: a blank body is not JSON, so it is
// already refused.
func TestCollectionSchemaStdin_BlankRefused(t *testing.T) {
	for name, body := range blankBodies {
		t.Run(name, func(t *testing.T) {
			if _, err := collectionSchemaJSONFromFlags("-", "", strings.NewReader(body)); err == nil {
				t.Fatal("a blank --schema - body must be refused")
			}
		})
	}
}
