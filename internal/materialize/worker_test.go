package materialize

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	pad "github.com/PerpetualSoftware/pad"
)

func frame(t *testing.T, v any) []byte {
	t.Helper()
	var body []byte
	switch x := v.(type) {
	case []byte:
		body = x
	default:
		var err error
		if body, err = json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
	out := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(out, uint32(len(body)))
	return append(out, body...)
}

func readResponses(t *testing.T, b []byte) []WorkerResponse {
	t.Helper()
	var out []WorkerResponse
	r := bytes.NewReader(b)
	for {
		p, err := readFrame(r)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("response stream: %v", err)
		}
		var resp WorkerResponse
		if err := json.Unmarshal(p, &resp); err != nil {
			t.Fatalf("response is not JSON: %v: %q", err, p)
		}
		out = append(out, resp)
	}
}

func TestWorkerRoundTrip(t *testing.T) {
	cases := loadCorpus(t)
	schema := runner(t).SchemaVersion()
	var in bytes.Buffer
	in.Write(frame(t, WorkerRequest{ID: 1, Rows: cases[0].Rows, SchemaVersion: schema, LinkIndex: cases[0].LinkIndex, WorkspaceSlug: cases[0].WorkspaceSlug, TimeoutMs: 30_000}))
	in.Write(frame(t, []byte("{not json")))                                                // malformed JSON
	in.Write(frame(t, WorkerRequest{ID: 3, Rows: []string{"!!!"}, SchemaVersion: schema})) // bad base64
	in.Write(frame(t, WorkerRequest{ID: 4, SchemaVersion: "999"}))                         // wrong schema
	in.Write(frame(t, WorkerRequest{ID: 5, SchemaVersion: schema}))                        // empty op-log -> ""
	in.Write(frame(t, WorkerRequest{ID: 6, SchemaVersion: schema, TimeoutMs: -1}))
	in.Write(frame(t, WorkerRequest{ID: 7, Rows: cases[1].Rows, SchemaVersion: schema, LinkIndex: cases[1].LinkIndex, WorkspaceSlug: cases[1].WorkspaceSlug}))

	var out bytes.Buffer
	if err := RunWorker(&in, &out, pad.MaterializerJS); err != nil {
		t.Fatalf("EOF at a frame boundary must end cleanly: %v", err)
	}
	resps := readResponses(t, out.Bytes())
	if len(resps) != 7 {
		t.Fatalf("got %d responses, want 7: %+v", len(resps), resps)
	}
	ok := func(i int, id uint64, want string) {
		r := resps[i]
		if r.ID != id || r.Error != "" || r.Markdown == nil || *r.Markdown != want {
			t.Errorf("response %d: %+v, want id %d markdown %q", i, r, id, want)
		}
	}
	fail := func(i int, id uint64, substr string) {
		r := resps[i]
		if r.ID != id || r.Markdown != nil || !strings.Contains(r.Error, substr) {
			t.Errorf("response %d: %+v, want id %d error containing %q", i, r, id, substr)
		}
	}
	ok(0, 1, cases[0].Expected)
	fail(1, 0, "malformed request")
	fail(2, 3, "not base64")
	fail(3, 4, "schema version mismatch")
	ok(4, 5, "")
	fail(5, 6, "timeout_ms")
	ok(6, 7, cases[1].Expected)
	// The empty-document answer is present-and-empty, not absent.
	if !bytes.Contains(out.Bytes(), []byte(`"id":5,"markdown":""`)) {
		t.Errorf("empty markdown not serialised as present: %s", out.Bytes())
	}
}

func TestWorkerEmptyInput(t *testing.T) {
	var out bytes.Buffer
	if err := serveWorker(bytes.NewReader(nil), &out, runner(t)); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("wrote %q with no requests", out.Bytes())
	}
}

func TestWorkerTruncatedFrame(t *testing.T) {
	good := frame(t, WorkerRequest{ID: 1, SchemaVersion: runner(t).SchemaVersion()})
	for name, in := range map[string][]byte{
		"header": append(append([]byte{}, good...), 0, 0),
		"body":   append(append([]byte{}, good...), good[:len(good)-3]...),
	} {
		var out bytes.Buffer
		err := serveWorker(bytes.NewReader(in), &out, runner(t))
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("%s: got %v, want ErrUnexpectedEOF", name, err)
		}
		if resps := readResponses(t, out.Bytes()); len(resps) != 1 || resps[0].ID != 1 {
			t.Errorf("%s: the complete request before the truncation must be answered: %+v", name, resps)
		}
	}
}

func TestWorkerOversizedFrame(t *testing.T) {
	in := make([]byte, 4)
	binary.BigEndian.PutUint32(in, MaxFrameBytes+1)
	var out bytes.Buffer
	if err := serveWorker(bytes.NewReader(in), &out, runner(t)); !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("got %v", err)
	}
	resps := readResponses(t, out.Bytes())
	if len(resps) != 1 || !strings.Contains(resps[0].Error, "exceeds") {
		t.Fatalf("got %+v", resps)
	}
}

func TestWorkerBadBundle(t *testing.T) {
	var out bytes.Buffer
	if err := RunWorker(bytes.NewReader(nil), &out, []byte("placeholder")); err == nil {
		t.Fatal("a bad bundle must fail before serving")
	}
	if out.Len() != 0 {
		t.Fatalf("wrote %q", out.Bytes())
	}
}
