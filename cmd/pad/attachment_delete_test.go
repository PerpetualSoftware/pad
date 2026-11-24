package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// `pad attachment delete`: HEAD each id for the filename and size, confirm
// unless --yes, attempt every id, and fail the command if any id failed.

type attachDeleteStub struct {
	missing map[string]bool // ids answered 404 on HEAD and DELETE
	denied  map[string]bool // ids answered 403 on DELETE

	mu    sync.Mutex
	calls []string
}

func (s *attachDeleteStub) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, r.Method+" "+r.URL.Path)
	id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	if s.missing[id] {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodHead:
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.png"`, id))
		w.Header().Set("Content-Length", "2048")
		w.Header().Set("Content-Type", "image/png")
	case http.MethodDelete:
		if s.denied[id] {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"not yours"}}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *attachDeleteStub) deletes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.calls {
		if strings.HasPrefix(c, "DELETE ") {
			out = append(out, c)
		}
	}
	return out
}

func runAttachmentDelete(t *testing.T, stub *attachDeleteStub, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	setupPushTest(t, http.HandlerFunc(stub.handler))
	cmd := attachmentDeleteCmd()
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetIn(strings.NewReader(stdin))
	var errBuf strings.Builder
	cmd.SetErr(&errBuf)
	stdout = captureStdout(t, func() { err = cmd.Execute() })
	return stdout, errBuf.String(), err
}

func TestAttachmentDelete_YesSkipsPromptAndSendsDelete(t *testing.T) {
	stub := &attachDeleteStub{}
	out, errOut, err := runAttachmentDelete(t, stub, "", "--yes", "a-1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(errOut, "[y/N]") {
		t.Errorf("--yes still prompted: %q", errOut)
	}
	want := "DELETE /api/v1/workspaces/demo/attachments/a-1"
	if d := stub.deletes(); len(d) != 1 || d[0] != want {
		t.Errorf("deletes = %v, want [%s]", d, want)
	}
	if !strings.Contains(out, "a-1.png") || !strings.Contains(out, "2.0 KB") {
		t.Errorf("stdout = %q, want filename and size", out)
	}
}

func TestAttachmentDelete_PromptDecline(t *testing.T) {
	stub := &attachDeleteStub{}
	out, errOut, err := runAttachmentDelete(t, stub, "n\n", "a-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "[y/N]") || !strings.Contains(errOut, "a-1.png") {
		t.Errorf("stderr = %q, want a prompt naming the file", errOut)
	}
	if d := stub.deletes(); len(d) != 0 {
		t.Errorf("declined but sent %v", d)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing", out)
	}
}

func TestAttachmentDelete_PromptAccept(t *testing.T) {
	stub := &attachDeleteStub{}
	if _, _, err := runAttachmentDelete(t, stub, "y\n", "a-1", "a-2"); err != nil {
		t.Fatal(err)
	}
	if d := stub.deletes(); len(d) != 2 {
		t.Errorf("deletes = %v, want two", d)
	}
}

func TestAttachmentDelete_NotFoundIsReportedAndFails(t *testing.T) {
	stub := &attachDeleteStub{missing: map[string]bool{"gone": true}}
	_, errOut, err := runAttachmentDelete(t, stub, "", "-y", "gone")
	if err == nil {
		t.Fatal("want a non-nil error for a missing attachment")
	}
	if !strings.Contains(errOut, "attachment gone not found in workspace demo") {
		t.Errorf("stderr = %q, want the named not-found", errOut)
	}
	if d := stub.deletes(); len(d) != 0 {
		t.Errorf("sent %v for an attachment that HEAD said is missing", d)
	}
}

func TestAttachmentDelete_OneFailureStillTriesTheRest(t *testing.T) {
	stub := &attachDeleteStub{
		missing: map[string]bool{"b-missing": true},
		denied:  map[string]bool{"c-denied": true},
	}
	out, errOut, err := runAttachmentDelete(t, stub, "", "-y", "a-ok", "b-missing", "c-denied", "d-ok")
	if err == nil || !strings.Contains(err.Error(), "2 of 4") {
		t.Fatalf("err = %v, want a 2 of 4 failure", err)
	}
	deletes := strings.Join(stub.deletes(), "\n")
	for _, id := range []string{"a-ok", "c-denied", "d-ok"} {
		if !strings.Contains(deletes, "/attachments/"+id) {
			t.Errorf("no DELETE for %s in %q", id, deletes)
		}
	}
	for _, id := range []string{"a-ok", "d-ok"} {
		if !strings.Contains(out, id+".png") {
			t.Errorf("stdout %q missing a deleted line for %s", out, id)
		}
	}
	if !strings.Contains(errOut, "b-missing: attachment b-missing not found") || !strings.Contains(errOut, "c-denied: not yours") {
		t.Errorf("stderr = %q, want both failures", errOut)
	}
}

func TestAttachmentDelete_RequiresAnID(t *testing.T) {
	stub := &attachDeleteStub{}
	if _, _, err := runAttachmentDelete(t, stub, ""); err == nil {
		t.Fatal("want an args error with no ids")
	}
}
