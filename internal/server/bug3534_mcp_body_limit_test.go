package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BUG-3534: the MCP mounts refuse a POST body over mcpBodyLimit with 413
// before anything reads it, on both the declared-length and the chunked
// path. Driven through the router so the middleware's place in each
// mount's chain is what is tested.

func postMCPSized(srv *Server, path, token string, n int64, chunked bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, io.LimitReader(repeatReader('x'), n))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if chunked {
		req.ContentLength = -1
	} else {
		req.ContentLength = n
	}
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// repeatReader is an endless stream of one byte, so a large body costs no
// memory to build.
type repeatReader byte

func (b repeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

func isBodyTooLarge(t *testing.T, rr *httptest.ResponseRecorder, limit int64) bool {
	t.Helper()
	if rr.Code != http.StatusRequestEntityTooLarge {
		return false
	}
	var env struct {
		JSONRPC string           `json:"jsonrpc"`
		ID      *json.RawMessage `json:"id"`
		Error   struct {
			Code int `json:"code"`
			Data struct {
				Code       string `json:"code"`
				LimitBytes int64  `json:"limit_bytes"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Errorf("413 body is not JSON: %q", rr.Body.String())
		return false
	}
	if env.JSONRPC != "2.0" || env.Error.Code != -32600 || env.Error.Data.Code != "too_large" || env.Error.Data.LimitBytes != limit {
		t.Errorf("413 body = %s", rr.Body.String())
	}
	return true
}

func TestBUG3534_DefaultLimitIsSevenMiB(t *testing.T) {
	srv := testServer(t)
	if got, want := srv.mcpBodyLimit(), int64(7<<20); got != want {
		t.Fatalf("mcpBodyLimit = %d, want %d", got, want)
	}
}

// At the limit, the request goes on to auth (no token: 401). One byte over,
// it is refused 413 before auth. Both mounts, both length paths.
func TestBUG3534_LimitOnBothMountsAndBothLengthPaths(t *testing.T) {
	srv := chatGPTMountServer(t, true)
	limit := srv.mcpBodyLimit()
	for _, path := range []string{"/mcp", "/mcp/chatgpt"} {
		for _, chunked := range []bool{false, true} {
			name := path + map[bool]string{false: " content-length", true: " chunked"}[chunked]
			if rr := postMCPSized(srv, path, "", limit, chunked); rr.Code != http.StatusUnauthorized {
				t.Errorf("%s at the limit: %d, want 401 from auth (the body passed)", name, rr.Code)
			}
			if rr := postMCPSized(srv, path, "", limit+1, chunked); !isBodyTooLarge(t, rr, limit) {
				t.Errorf("%s one byte over: %d %q, want 413", name, rr.Code, rr.Body.String())
			}
		}
	}
}

// Within the limit, the transport receives the body byte for byte, on the
// chunked path too, which is the one this middleware reads and replays.
func TestBUG3534_BodyWithinTheLimitArrivesIntact(t *testing.T) {
	srv, rt := scopeTestServer(t)
	tok, code := mintWithResourceTier(t, srv, newOAuthSession(t, srv), testCanonicalAudience, "pad:read pad:write", "write")
	if code != http.StatusOK {
		t.Fatalf("mint: %d", code)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"pad_item","arguments":{"action":"create","content":"` +
		strings.Repeat("y", 3<<20) + `"}}}`
	for _, chunked := range []bool{false, true} {
		req := httptest.NewRequest("POST", "/mcp", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		if chunked {
			req.ContentLength = -1
		}
		req.RemoteAddr = "192.0.2.1:1234"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || rr.Body.String() != "mcp" {
			t.Fatalf("chunked=%v: %d %q, want 200 from the transport", chunked, rr.Code, rr.Body.String())
		}
		if got := rt.take(); len(got) != 1 || got[0] != body {
			t.Errorf("chunked=%v: the transport did not receive the body intact", chunked)
		}
	}
}

// The limit follows the operator's artifact cap, so raising that cap never
// makes remote import refuse a legal artifact at this door first.
func TestBUG3534_LimitFollowsTheArtifactCap(t *testing.T) {
	srv := chatGPTMountServer(t, true)
	srv.SetImportArtifactMaxBytes(4 << 20)
	limit := srv.mcpBodyLimit()
	if want := int64(6*(4<<20) + 1<<20); limit != want {
		t.Fatalf("mcpBodyLimit with a 4 MiB artifact cap = %d, want %d", limit, want)
	}
	if rr := postMCPSized(srv, "/mcp", "", 10<<20, false); rr.Code != http.StatusUnauthorized {
		t.Errorf("10 MiB under a 25 MiB limit: %d, want 401 from auth", rr.Code)
	}
	if rr := postMCPSized(srv, "/mcp", "", limit+1, false); !isBodyTooLarge(t, rr, limit) {
		t.Errorf("one byte over the raised limit: %d", rr.Code)
	}
}
