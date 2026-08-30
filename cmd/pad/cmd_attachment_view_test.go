package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// attachmentViewTestHandler serves HEAD/GET for any path under /attachments/,
// with a fixed Content-Type and optional Content-Disposition. Accepting any
// suffix is deliberate: the traversal case below needs the server to answer
// for a hostile id the way a re-routed request would.
func attachmentViewTestHandler(disposition, mime string, body []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/attachments/") {
			http.NotFound(w, r)
			return
		}
		if disposition != "" {
			w.Header().Set("Content-Disposition", disposition)
		}
		w.Header().Set("Content-Type", mime)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(body)
	})
}

// runAttachmentView executes `pad attachment view <id>` against a stub server
// and returns the path the command printed. The command prints via
// fmt.Println(os.Stdout) so $(...) substitution works; the test captures it
// the same way a shell would.
func runAttachmentView(t *testing.T, handler http.Handler, id string) string {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	t.Setenv("HOME", t.TempDir())
	// Sandbox the command's os.MkdirTemp so the containment assertion below
	// has a boundary this test owns.
	t.Setenv("TMPDIR", t.TempDir())

	previousWorkspace := workspaceFlag
	previousURL := urlFlag
	workspaceFlag = "demo"
	urlFlag = server.URL + "/"
	t.Cleanup(func() {
		workspaceFlag = previousWorkspace
		urlFlag = previousURL
	})

	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	oldStdout := os.Stdout
	os.Stdout = wr
	defer func() { os.Stdout = oldStdout }()

	cmd := attachmentViewCmd()
	cmd.SetArgs([]string{id})
	execErr := cmd.Execute()

	_ = wr.Close()
	os.Stdout = oldStdout
	out := make([]byte, 4096)
	n, _ := rd.Read(out)
	_ = rd.Close()

	if execErr != nil {
		t.Fatalf("execute attachment view: %v", execErr)
	}
	return strings.TrimSpace(string(out[:n]))
}

// TestAttachmentViewNamesTheFileThroughTheCommand exercises the WIRING —
// helper tests alone left the command's naming fallback an untested claim
// (codex closing round 3, CONVE-19's direct-call-vouches-for-the-component
// point).
func TestAttachmentViewNamesTheFileThroughTheCommand(t *testing.T) {
	body := []byte("png-bytes")

	t.Run("disposition name used verbatim", func(t *testing.T) {
		got := runAttachmentView(t,
			attachmentViewTestHandler(`attachment; filename="shot.png"`, "image/png", body),
			"11111111-1111-1111-1111-111111111111")
		if filepath.Base(got) != "shot.png" {
			t.Fatalf("printed path %q, want basename shot.png", got)
		}
		assertWrittenInsideTemp(t, got, body)
	})

	t.Run("extensionless stored name gains the MIME extension", func(t *testing.T) {
		got := runAttachmentView(t,
			attachmentViewTestHandler(`attachment; filename="upload"`, "image/png", body),
			"22222222-2222-2222-2222-222222222222")
		if filepath.Base(got) != "upload.png" {
			t.Fatalf("printed path %q, want basename upload.png", got)
		}
		assertWrittenInsideTemp(t, got, body)
	})

	t.Run("absent name falls back to the id plus extension", func(t *testing.T) {
		got := runAttachmentView(t,
			attachmentViewTestHandler("", "image/png", body),
			"33333333-3333-3333-3333-333333333333")
		if filepath.Base(got) != "33333333-3333-3333-3333-333333333333.png" {
			t.Fatalf("printed path %q, want id-based basename", got)
		}
		assertWrittenInsideTemp(t, got, body)
	})

	// The closing-round-3 defect: an id harvested from item content can be
	// traversal-shaped. Unfixed, the fallback joined it raw onto the temp
	// dir and the write escaped it. The stub answers for ANY attachments
	// path, standing in for a re-routed request that answers 200.
	t.Run("traversal-shaped id cannot escape the temp dir", func(t *testing.T) {
		var uris []string
		record := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uris = append(uris, r.RequestURI)
			attachmentViewTestHandler("", "image/png", body).ServeHTTP(w, r)
		})
		got := runAttachmentView(t, record, "../../escape")
		if filepath.Base(got) != "escape.png" {
			t.Fatalf("printed path %q, want the reduced basename escape.png", got)
		}
		assertWrittenInsideTemp(t, got, body)
		// The client must PATH-ESCAPE the id so the request cannot be
		// re-routed off the attachments route by dot segments — the wire
		// half of the same defect. Without the escaping, this wire either
		// carries a raw "../" or a cleaned path with the segments gone.
		for _, uri := range uris {
			if !strings.Contains(uri, "%2F") || strings.Contains(uri, "../") {
				t.Fatalf("request URI %q carries unescaped dot segments; the id must be path-escaped", uri)
			}
		}
		if len(uris) == 0 {
			t.Fatal("premise failed: the stub saw no requests")
		}
	})

	t.Run("unusable id falls back to the generic name", func(t *testing.T) {
		got := runAttachmentView(t,
			attachmentViewTestHandler("", "image/png", body),
			"....")
		if filepath.Base(got) != "attachment.png" {
			t.Fatalf("printed path %q, want attachment.png", got)
		}
		assertWrittenInsideTemp(t, got, body)
	})
}

// assertWrittenInsideTemp asserts the consequence a wrong join would produce:
// a file OUTSIDE the sandboxed TMPDIR (or none at the printed path at all).
func assertWrittenInsideTemp(t *testing.T, printed string, want []byte) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(os.Getenv("TMPDIR"))
	if err != nil {
		t.Fatalf("resolve TMPDIR: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(printed)
	if err != nil {
		t.Fatalf("printed path %q does not resolve: %v", printed, err)
	}
	if !strings.HasPrefix(resolved, tmp+string(filepath.Separator)) {
		t.Fatalf("file %q landed outside the sandboxed temp dir %q", resolved, tmp)
	}
	data, err := os.ReadFile(printed)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if string(data) != string(want) {
		t.Fatalf("written bytes = %q, want %q", data, want)
	}
}
