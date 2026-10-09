package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3534: the MCP mounts refuse an authenticated POST body over
// mcpBodyLimit with 413 before the transport reads it, on both the
// declared-length and the chunked path; an unauthenticated body is never
// read. Driven through the router so the middleware's place in each
// mount's chain is what is tested.

func postMCPSized(srv *Server, path, token string, n int64, chunked bool) *httptest.ResponseRecorder {
	rr, _ := postMCPSizedCounting(srv, path, token, n, chunked)
	return rr
}

// postMCPSizedCounting also reports how many body bytes anything read.
func postMCPSizedCounting(srv *Server, path, token string, n int64, chunked bool) (*httptest.ResponseRecorder, int64) {
	body := &mcpBodyCountingReader{r: io.LimitReader(repeatReader('x'), n)}
	req := httptest.NewRequest("POST", path, body)
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
	return rr, body.n
}

type mcpBodyCountingReader struct {
	r io.Reader
	n int64
}

func (c *mcpBodyCountingReader) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	c.n += int64(k)
	return k, err
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

// At the limit the request reaches the mount's transport; one byte over,
// it is refused 413 before the transport. Both mounts, both length paths,
// each with a token for its own mount.
func TestBUG3534_LimitOnBothMountsAndBothLengthPaths(t *testing.T) {
	srv := chatGPTMountServer(t, true)
	sess := newOAuthSession(t, srv)
	mcpTok, _ := mintWithResource(t, srv, sess, testCanonicalAudience)
	gptTok, _ := mintWithResource(t, srv, sess, testChatGPTResource)
	limit := srv.mcpBodyLimit()
	for _, m := range []struct{ path, token, stub string }{
		{"/mcp", mcpTok, "mcp"},
		{"/mcp/chatgpt", gptTok, "chatgpt"},
	} {
		for _, chunked := range []bool{false, true} {
			name := m.path + map[bool]string{false: " content-length", true: " chunked"}[chunked]
			if rr := postMCPSized(srv, m.path, m.token, limit, chunked); rr.Code != http.StatusOK || rr.Body.String() != m.stub {
				t.Errorf("%s at the limit: %d %q, want the transport's 200", name, rr.Code, rr.Body.String())
			}
			if rr := postMCPSized(srv, m.path, m.token, limit+1, chunked); !isBodyTooLarge(t, rr, limit) {
				t.Errorf("%s one byte over: %d %q, want 413", name, rr.Code, rr.Body.String())
			}
		}
	}
}

// Nothing reads an unauthenticated body (codex review): an oversized one,
// with or without a declared length, is refused 401 by auth, which the
// pre-auth rate limit meters, with zero body bytes read.
func TestBUG3534_UnauthenticatedBodyIsNeverRead(t *testing.T) {
	srv := chatGPTMountServer(t, true)
	limit := srv.mcpBodyLimit()
	for _, path := range []string{"/mcp", "/mcp/chatgpt"} {
		for _, chunked := range []bool{false, true} {
			rr, read := postMCPSizedCounting(srv, path, "", limit+1, chunked)
			if rr.Code != http.StatusUnauthorized || read != 0 {
				t.Errorf("%s chunked=%v without a token: %d after reading %d body bytes, want 401 and 0", path, chunked, rr.Code, read)
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
	tok, _ := mintWithResource(t, srv, newOAuthSession(t, srv), testCanonicalAudience)
	limit := srv.mcpBodyLimit()
	if want := int64(6*(4<<20) + 1<<20); limit != want {
		t.Fatalf("mcpBodyLimit with a 4 MiB artifact cap = %d, want %d", limit, want)
	}
	if rr := postMCPSized(srv, "/mcp", tok, 10<<20, false); rr.Code != http.StatusOK {
		t.Errorf("10 MiB under a 25 MiB limit: %d, want the transport's 200", rr.Code)
	}
	if rr := postMCPSized(srv, "/mcp", tok, limit+1, false); !isBodyTooLarge(t, rr, limit) {
		t.Errorf("one byte over the raised limit: %d", rr.Code)
	}
}

// The 413 is audited, as the response of an authenticated call
// (codex round 2): one row, classified error / client_error_413 by
// classifyMCPResult like any other 4xx that is not an auth or rate gate.
func TestBUG3534_TooLargeIsAudited(t *testing.T) {
	srv, user, bearer := auditedMCPServer(t)
	limit := srv.mcpBodyLimit()
	if rr := postMCPSized(srv, "/mcp", bearer, limit+1, false); !isBodyTooLarge(t, rr, limit) {
		t.Fatalf("over the limit: %d", rr.Code)
	}
	rows := waitForAuditRows(t, srv, user.ID, 1)
	if len(rows) != 1 || rows[0].ResultStatus != models.MCPAuditResultError || rows[0].ErrorKind == nil || *rows[0].ErrorKind != "client_error_413" {
		t.Fatalf("audit rows = %+v, want one error / client_error_413", rows)
	}
}
