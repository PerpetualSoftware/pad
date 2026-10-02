package mcp

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TASK-3321 U2c: WithChatGPTToolSchemes rewrites exactly one thing, a
// tools/list JSON response, and hands everything else through byte for
// byte, request body included.

const listWithMeta = `{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"a","_meta":{"securitySchemes":[{"type":"oauth2","scopes":["pad:read"]}]}},{"name":"b"}]}}` + "\n"

// stub answers with fixed bytes and records the request body it read.
func stubTransport(contentType string, status int, answer string, seen *[]byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	})
}

func serveSchemes(h http.Handler, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	return rr
}

func TestWithChatGPTToolSchemes_LiftsOnToolsList(t *testing.T) {
	var seen []byte
	req := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	rr := serveSchemes(WithChatGPTToolSchemes(stubTransport("application/json", 200, listWithMeta, &seen)), req)
	if string(seen) != req {
		t.Fatalf("the transport read %q, want the request body intact", seen)
	}
	want := `{"id":2,"jsonrpc":"2.0","result":{"tools":[{"_meta":{"securitySchemes":[{"type":"oauth2","scopes":["pad:read"]}]},"name":"a","securitySchemes":[{"type":"oauth2","scopes":["pad:read"]}]},{"name":"b"}]}}` + "\n"
	if rr.Body.String() != want {
		t.Fatalf("got  %s\nwant %s", rr.Body.String(), want)
	}
}

func TestWithChatGPTToolSchemes_PassesEverythingElseThrough(t *testing.T) {
	list := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	cases := []struct {
		name, body, contentType string
		status                  int
	}{
		{"another method", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"a"}}`, "application/json", 200},
		{"a batch", "[" + list + "]", "application/json", 200},
		{"an oversized body", `{"jsonrpc":"2.0","id":2,"method":"tools/list","pad":"` + strings.Repeat("x", chatGPTListBodyLimit) + `"}`, "application/json", 200},
		{"an event stream", list, "text/event-stream", 200},
		{"a refusal", list, "application/json", 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var seen []byte
			rr := serveSchemes(WithChatGPTToolSchemes(stubTransport(c.contentType, c.status, listWithMeta, &seen)), c.body)
			if !bytes.Equal(seen, []byte(c.body)) {
				t.Errorf("the transport read %d bytes, want the %d-byte body intact", len(seen), len(c.body))
			}
			if rr.Code != c.status || rr.Body.String() != listWithMeta {
				t.Errorf("response changed: %d %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// The writer WithChatGPTToolSchemes hands the transport must not stream:
// mcp-go upgrades a response to an event stream only through a writer that
// can flush, and a streamed tools/list would leave the rewrite nothing to
// rewrite.
func TestBufferedResponseCannotStream(t *testing.T) {
	if _, ok := any(&bufferedResponse{}).(http.Flusher); ok {
		t.Fatal("bufferedResponse implements http.Flusher, so tools/list could be answered as an event stream")
	}
}
