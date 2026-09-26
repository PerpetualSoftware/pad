package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
)

// formBodyParseCap is net/http's own cap on a form-encoded body
// (parsePostForm's maxFormSize when the body is not a MaxBytesReader).
// Reading exactly as far as ParseForm would is what lets this check see
// every byte a handler could see without buffering anything it would not.
const formBodyParseCap = 10 << 20

// ValidateFormBody is the form-encoded-body half of the transport text rule
// ValidatePath (BUG-2782) and ValidateQuery (BUG-2784) apply: a request whose
// application/x-www-form-urlencoded body carries a key or value that is not
// bindableText is refused with 400 before the handler runs.
//
// WHY IT EXISTS. r.Form merges the query string with the form body, and
// ValidateQuery can only see the query half. Measured on BUG-2811 at
// bd21b97c, with the fosite-backed OAuth server wired and every control leg
// reaching its handler: a NUL in connection_name (/oauth/authorize/decide),
// code or refresh_token (/oauth/token) answered 500 on SQLite and Postgres
// 17; invalid UTF-8 in the same three answered 500 on Postgres and was
// STORED or refused differently on SQLite — the dialect split the query rule
// exists to close. client_id, the Basic-auth client id, state and the revoke
// token answered non-500 on both dialects; they are covered here because the
// rule is per request, not per parameter.
//
// WHY THE OAUTH ROUTES AND NOT THE ROUTER ROOT. The OAuth POST handlers are
// the only consumers of form-encoded bodies in the product (multipart
// attachments are a different content type). Mounted at the root, this would
// buffer up to 10 MiB for any route receiving a form content type, including
// JSON routes whose own caps are far lower, before authentication — an
// amplification those routes do not have today. Scoped to the routes that
// call ParseForm, it reads exactly what ParseForm was going to read anyway.
//
// FIDELITY TO WHAT THE HANDLER READS. The content-type gate is
// parsePostForm's: mime.ParseMediaType's media type is compared regardless
// of its error, because parsePostForm parses the body in that case too.
// The body is decoded by validQueryText, which calls url.ParseQuery — the
// same call parsePostForm makes — so the pairs checked are the pairs the
// handler gets. The bytes read are handed back to the handler unchanged:
//   - a body over the cap is not checked, and the handler's own ParseForm
//     then reads past its cap and refuses it as too large, as it always did;
//   - a read error is replayed to the handler after the bytes that did
//     arrive, so its ParseForm fails the way it would have.
//
// The one divergence: parsePostForm skips its cap when the body is already a
// MaxBytesReader. No OAuth route installs one, so it does not arise here.
//
// The refusal is RFC 6749 §5.2's error shape rather than the API envelope,
// because /oauth/token, /revoke and /introspect speak it to OAuth clients.
func ValidateFormBody(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			next(w, r)
			return
		}
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
		default:
			next(w, r)
			return
		}
		if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/x-www-form-urlencoded" {
			next(w, r)
			return
		}
		orig := r.Body
		buf, readErr := io.ReadAll(io.LimitReader(orig, formBodyParseCap+1))
		switch {
		case readErr != nil:
			r.Body = replayBody{io.MultiReader(bytes.NewReader(buf), errReader{readErr}), orig}
		case int64(len(buf)) > formBodyParseCap:
			r.Body = replayBody{io.MultiReader(bytes.NewReader(buf), orig), orig}
		default:
			if !validQueryText(string(buf)) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error":             "invalid_request",
					"error_description": "Request body contains invalid UTF-8 or a NUL byte",
				})
				return
			}
			r.Body = replayBody{bytes.NewReader(buf), orig}
		}
		next(w, r)
	}
}

// replayBody serves already-read bytes (and whatever follows them) while
// closing the original body, so the server's connection handling is
// unchanged.
type replayBody struct {
	io.Reader
	orig io.Closer
}

func (b replayBody) Close() error { return b.orig.Close() }

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
